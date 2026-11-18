// Package admin is the local control channel between the running server and
// the wa-mcp CLI (pair, accounts, token, remove). It is an HTTP API on a Unix
// socket inside the data directory, so access is governed by file
// permissions and it is never reachable from the network.
package admin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/gerswin/whatsapp-mcp/internal/wa"
)

const SocketName = "admin.sock"

// Listen creates the admin socket. It fails if the path is not usable (for
// example, longer than the ~100 characters Unix sockets allow), which is
// worth knowing at startup: without it accounts cannot be linked.
func Listen(socketPath string) (net.Listener, error) {
	_ = os.Remove(socketPath) // stale socket from a previous run
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("create admin socket at %s: %w", socketPath, err)
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// Serve answers CLI requests on ln until ctx is done.
func Serve(ctx context.Context, ln net.Listener, mgr *wa.Manager) error {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /pair", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		enc := json.NewEncoder(w)
		flush := func() {
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}

		// r.Context() ends when the CLI goes away, which aborts the attempt.
		err := mgr.Pair(r.Context(), r.URL.Query().Get("name"), r.URL.Query().Get("phone"), func(evt wa.PairEvent) {
			_ = enc.Encode(evt)
			flush()
		})
		if err != nil {
			_ = enc.Encode(map[string]string{"type": "error", "error": err.Error()})
			flush()
		}
	})

	mux.HandleFunc("GET /accounts", func(w http.ResponseWriter, _ *http.Request) {
		statuses, err := mgr.Statuses()
		reply(w, statuses, err)
	})

	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		token, err := mgr.RotateToken(r.URL.Query().Get("name"))
		reply(w, map[string]string{"token": token}, err)
	})

	mux.HandleFunc("POST /remove", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		reply(w, map[string]bool{"ok": true}, mgr.Remove(ctx, r.URL.Query().Get("name")))
	})

	srv := &http.Server{Handler: mux}
	go func() {
		<-ctx.Done()
		srv.Close() // closing the listener also removes the socket file
	}()

	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func reply(w http.ResponseWriter, v any, err error) {
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(v)
}

// Client talks to a running server through its admin socket.
type Client struct {
	http *http.Client
}

func NewClient(socketPath string) (*Client, error) {
	if _, err := os.Stat(socketPath); err != nil {
		return nil, fmt.Errorf("the server is not running (no admin socket at %s); start it with \"wa-mcp serve\"", socketPath)
	}
	return &Client{http: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		},
	}}}, nil
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://admin"+path+"?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach the server: %w", err)
	}
	return resp, nil
}

func (c *Client) json(ctx context.Context, method, path string, query url.Values, out any) error {
	resp, err := c.do(ctx, method, path, query)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &e)
		if e.Error == "" {
			e.Error = fmt.Sprintf("server answered %d", resp.StatusCode)
		}
		return errors.New(e.Error)
	}
	return json.Unmarshal(body, out)
}

// PairEvent is a wa.PairEvent, or an error reported by the server.
type PairEvent struct {
	wa.PairEvent
	Error string `json:"error,omitempty"`
}

// Pair runs a pairing attempt, calling onEvent for every step.
func (c *Client) Pair(ctx context.Context, name, phone string, onEvent func(PairEvent)) error {
	resp, err := c.do(ctx, http.MethodPost, "/pair", url.Values{"name": {name}, "phone": {phone}})
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	done := false
	for scanner.Scan() {
		var evt PairEvent
		if err := json.Unmarshal(scanner.Bytes(), &evt); err != nil {
			continue
		}
		if evt.Type == "error" {
			return errors.New(evt.Error)
		}
		if evt.Type == "success" {
			done = true
		}
		onEvent(evt)
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if !done {
		return errors.New("the server closed the connection before pairing finished")
	}
	return nil
}

func (c *Client) Accounts(ctx context.Context) ([]wa.AccountStatus, error) {
	var out []wa.AccountStatus
	err := c.json(ctx, http.MethodGet, "/accounts", nil, &out)
	return out, err
}

func (c *Client) RotateToken(ctx context.Context, name string) (string, error) {
	var out struct {
		Token string `json:"token"`
	}
	err := c.json(ctx, http.MethodPost, "/token", url.Values{"name": {name}}, &out)
	return out.Token, err
}

func (c *Client) Remove(ctx context.Context, name string) error {
	var out map[string]bool
	return c.json(ctx, http.MethodPost, "/remove", url.Values{"name": {name}}, &out)
}
