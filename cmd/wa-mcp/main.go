// wa-mcp connects WhatsApp accounts to assistants such as Claude or ChatGPT
// through the Model Context Protocol.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/skip2/go-qrcode"

	"github.com/gerswin/whatsapp-mcp/internal/admin"
	"github.com/gerswin/whatsapp-mcp/internal/mcpserver"
	"github.com/gerswin/whatsapp-mcp/internal/media"
	"github.com/gerswin/whatsapp-mcp/internal/store"
	"github.com/gerswin/whatsapp-mcp/internal/wa"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `wa-mcp — WhatsApp for Claude, ChatGPT and other MCP clients

Usage:
  wa-mcp serve                     run the server
  wa-mcp pair <name> [flags]       link a WhatsApp account (the server must be running)
        --phone <number>           link with a code typed on the phone instead of a QR
        --light                    draw the QR for a light terminal background
  wa-mcp accounts                  list accounts and their connection state
  wa-mcp token <name>              issue a new token for an account (the old one stops working)
  wa-mcp remove <name>             unlink an account and delete its stored messages
  wa-mcp version

Configuration (environment):
  DATA_DIR              where the databases live (default ./data)
  LISTEN_ADDR           address to listen on (default 127.0.0.1:4020)
  PUBLIC_URL            public https origin, e.g. https://wa.example.com (needed for OAuth
                        unless a proxy or tunnel forwards the Host header)
  RETENTION_DAYS        delete stored messages older than this (default 0 = keep forever)
  WHISPER_API_KEY       enables audio transcription (OPENAI_API_KEY is also accepted)
  WHISPER_BASE_URL      OpenAI-compatible API base (default https://api.openai.com/v1)
  WHISPER_MODEL         transcription model (default whisper-1)
  WHISPER_LANGUAGE      language hint such as "es" (default: auto-detect)
  WHISPER_AUTO          transcribe incoming audio automatically (default true)
  STORE_STATUS          keep contacts' status updates as a chat (default false)
  WHISPER_MAX_SECONDS   longest audio to transcribe automatically (default 600)
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "serve":
		err = serve()
	case "pair":
		err = pair(os.Args[2:])
	case "accounts":
		err = accounts()
	case "token":
		err = token(os.Args[2:])
	case "remove":
		err = remove(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println(version)
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if n, err := strconv.Atoi(env(key, "")); err == nil {
		return n
	}
	return def
}

func dataDir() string { return env("DATA_DIR", "./data") }

// ------------------------------------------------------------------ serve

func serve() error {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(log)

	dir := dataDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}

	st, err := store.Open(filepath.Join(dir, "messages.db"))
	if err != nil {
		return err
	}
	defer st.Close()

	var transcriber wa.Transcriber
	if key := env("WHISPER_API_KEY", env("OPENAI_API_KEY", "")); key != "" {
		transcriber = media.NewWhisper(env("WHISPER_BASE_URL", ""), key, env("WHISPER_MODEL", ""), env("WHISPER_LANGUAGE", ""))
	}
	auto := transcriber != nil && !strings.EqualFold(env("WHISPER_AUTO", "true"), "false")
	storeStatus := strings.EqualFold(env("STORE_STATUS", "false"), "true")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	mgr, err := wa.NewManager(ctx, wa.Options{
		DataDir:        dir,
		Store:          st,
		Transcriber:    transcriber,
		AutoTranscribe: auto,
		AutoMaxSeconds: envInt("WHISPER_MAX_SECONDS", 600),
		StoreStatus:    storeStatus,
		Log:            log,
	})
	if err != nil {
		return err
	}
	defer mgr.Stop()

	if err := mgr.Start(ctx); err != nil {
		return err
	}

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	r.GET("/healthz", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true, "version": version}) })

	revoke, err := mcpserver.Register(r, mcpserver.Config{
		Backend:       mgr,
		Accounts:      mgr,
		Store:         st,
		OAuthDBPath:   filepath.Join(dir, "oauth.db"),
		PublicURL:     env("PUBLIC_URL", ""),
		Version:       version,
		Transcription: transcriber != nil,
		StatusUpdates: storeStatus,
	})
	if err != nil {
		return fmt.Errorf("start MCP server: %w", err)
	}
	// A removed account must not leave usable OAuth grants behind.
	mgr.OnRemove = func(account string) {
		if err := revoke(account); err != nil {
			log.Error("could not revoke OAuth grants", "account", account, "err", err)
		}
	}

	adminListener, err := admin.Listen(filepath.Join(dir, admin.SocketName))
	if err != nil {
		return err
	}
	go func() {
		if err := admin.Serve(ctx, adminListener, mgr); err != nil {
			log.Error("admin socket stopped", "err", err)
		}
	}()

	if days := envInt("RETENTION_DAYS", 0); days > 0 {
		go pruneLoop(ctx, st, days, log)
	}

	addr := env("LISTEN_ADDR", "127.0.0.1:4020")
	srv := &http.Server{Addr: addr, Handler: r, ReadHeaderTimeout: 15 * time.Second}

	failed := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			failed <- err
		}
	}()

	transcription := "off (set WHISPER_API_KEY to enable)"
	if transcriber != nil {
		transcription = "on demand"
		if auto {
			transcription = "automatic"
		}
	}
	log.Info("wa-mcp is running", "version", version, "listen", addr, "mcp", mcpserver.Path, "data", dir, "transcription", transcription, "status_updates", storeStatus)

	select {
	case err := <-failed:
		return err
	case <-ctx.Done():
	}

	log.Info("shutting down")
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}

func pruneLoop(ctx context.Context, st *store.Store, days int, log *slog.Logger) {
	ticker := time.NewTicker(6 * time.Hour)
	defer ticker.Stop()
	for {
		if n, err := st.Prune(time.Now().AddDate(0, 0, -days)); err != nil {
			log.Error("could not prune old messages", "err", err)
		} else if n > 0 {
			log.Info("pruned old messages", "count", n, "older_than_days", days)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// ------------------------------------------------------------------ CLI

func adminClient() (*admin.Client, error) {
	return admin.NewClient(filepath.Join(dataDir(), admin.SocketName))
}

func pair(args []string) error {
	fs := flag.NewFlagSet("pair", flag.ContinueOnError)
	phone := fs.String("phone", "", "link with a code typed on this phone number instead of a QR")
	light := fs.Bool("light", false, "draw the QR for a light terminal background")

	// Accept the name before or after the flags.
	var name string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if name == "" {
		name = fs.Arg(0)
	}
	if !wa.ValidAccountName(name) {
		return errors.New("usage: wa-mcp pair <name> [--phone <number>]  (names: lowercase letters, digits, '-' and '_')")
	}

	client, err := adminClient()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return client.Pair(ctx, name, *phone, func(evt admin.PairEvent) {
		switch evt.Type {
		case "qr":
			qr, err := qrcode.New(evt.Code, qrcode.Low)
			if err != nil {
				fmt.Println("could not draw the QR code:", err)
				return
			}
			fmt.Print("\n", qr.ToSmallString(*light))
			fmt.Println("On your phone: WhatsApp → Settings → Linked devices → Link a device, and scan this code.")
			fmt.Println("A new code appears here every few seconds until you scan one.")
		case "code":
			fmt.Printf("\nPairing code: %s-%s\n\n", evt.Code[:len(evt.Code)/2], evt.Code[len(evt.Code)/2:])
			fmt.Println("On your phone: WhatsApp → Settings → Linked devices → Link a device →")
			fmt.Println("\"Link with phone number instead\", and type the code.")
		case "success":
			fmt.Printf("\nLinked: account %q is now %s\n", name, evt.JID)
			if evt.Token != "" {
				fmt.Printf("\nAccount token (shown only once, keep it safe):\n\n    %s\n\n", evt.Token)
				fmt.Println("Paste it on the authorization page when you add the connector in Claude or ChatGPT,")
				fmt.Println("or send it as \"Authorization: Bearer <token>\" from clients that support headers.")
			} else {
				fmt.Println("The account keeps its existing token. Run \"wa-mcp token " + name + "\" if you need a new one.")
			}
		}
	})
}

func accounts() error {
	client, err := adminClient()
	if err != nil {
		return err
	}
	list, err := client.Accounts(context.Background())
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Println("No accounts yet. Link one with: wa-mcp pair <name>")
		return nil
	}

	fmt.Printf("%-20s %-14s %-32s %s\n", "ACCOUNT", "STATE", "WHATSAPP", "NAME")
	for _, a := range list {
		state := "not linked"
		switch {
		case a.Connected && a.LoggedIn:
			state = "connected"
		case a.JID != "":
			state = "disconnected"
		}
		fmt.Printf("%-20s %-14s %-32s %s\n", a.Account, state, a.JID, a.Name)
	}
	return nil
}

func token(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: wa-mcp token <name>")
	}
	client, err := adminClient()
	if err != nil {
		return err
	}
	tok, err := client.RotateToken(context.Background(), args[0])
	if err != nil {
		return err
	}
	fmt.Printf("New token for %q (the previous one no longer works):\n\n    %s\n\n", args[0], tok)
	fmt.Println("Connectors that were already authorized keep working; the token is only needed to authorize new ones.")
	return nil
}

func remove(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: wa-mcp remove <name>")
	}
	client, err := adminClient()
	if err != nil {
		return err
	}
	if err := client.Remove(context.Background(), args[0]); err != nil {
		return err
	}
	fmt.Printf("Account %q was unlinked from WhatsApp and its stored messages were deleted.\n", args[0])
	return nil
}
