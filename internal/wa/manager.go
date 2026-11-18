// Package wa owns the WhatsApp side: one whatsmeow client per account,
// pairing, message capture into the store, and the read/send operations the
// MCP tools are built on.
package wa

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCompanionReg"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	_ "modernc.org/sqlite"

	msgstore "github.com/gerswin/whatsapp-mcp/internal/store"
)

// Transcriber turns recorded speech into text (see internal/media.Whisper).
type Transcriber interface {
	Transcribe(ctx context.Context, data []byte, mime string) (string, error)
}

type Options struct {
	// DataDir holds whatsapp.db, the WhatsApp session database.
	DataDir string
	Store   *msgstore.Store
	// Transcriber is nil when speech-to-text is not configured.
	Transcriber Transcriber
	// AutoTranscribe transcribes voice notes and audio as they arrive, up to
	// AutoMaxSeconds long, so they show up in history and search.
	AutoTranscribe bool
	AutoMaxSeconds int
	// StoreStatus keeps contacts' status updates as a chat of their own.
	StoreStatus bool
	Log         *slog.Logger
}

type Manager struct {
	store          *msgstore.Store
	container      *sqlstore.Container
	db             *sql.DB
	transcriber    Transcriber
	autoEnabled    bool
	autoMaxSeconds int
	storeStatus    bool
	log            *slog.Logger

	mu      sync.RWMutex
	clients map[string]*whatsmeow.Client

	// OnRemove, when set, is called after an account has been removed.
	OnRemove func(account string)

	// allowPrivateFetch lets tests download from a local HTTP server.
	allowPrivateFetch bool

	// pairing serializes Pair calls: two concurrent attempts for the same
	// account would race for the same slot.
	pairing sync.Mutex
}

const tokenPrefix = "wam_"

var accountName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

func NewManager(ctx context.Context, opts Options) (*Manager, error) {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.AutoMaxSeconds <= 0 {
		opts.AutoMaxSeconds = 600
	}

	// _txlock=immediate makes write transactions take the lock up front, so
	// concurrent accounts wait on busy_timeout instead of failing mid-way.
	dsn := "file:" + filepath.Join(opts.DataDir, "whatsapp.db") +
		"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open WhatsApp session database: %w", err)
	}

	container := sqlstore.NewWithDB(db, "sqlite3", waLog.Stdout("wa/db", "WARN", false))
	if err := container.Upgrade(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate WhatsApp session database: %w", err)
	}

	// What the phone shows under "Linked devices": "Google Chrome (Linux)".
	// Without a platform type WhatsApp lists the session as "Other device".
	// This is sent when pairing, so it only changes for newly linked devices.
	store.SetOSInfo("Linux", [3]uint32{1, 0, 0})
	store.DeviceProps.PlatformType = waCompanionReg.DeviceProps_CHROME.Enum()

	return &Manager{
		store:          opts.Store,
		container:      container,
		db:             db,
		transcriber:    opts.Transcriber,
		autoEnabled:    opts.AutoTranscribe && opts.Transcriber != nil,
		autoMaxSeconds: opts.AutoMaxSeconds,
		storeStatus:    opts.StoreStatus,
		log:            opts.Log,
		clients:        map[string]*whatsmeow.Client{},
	}, nil
}

// Start connects every account that has a paired device.
func (m *Manager) Start(ctx context.Context) error {
	accounts, err := m.store.Accounts()
	if err != nil {
		return err
	}

	for _, a := range accounts {
		if a.JID == "" {
			m.log.Warn("account is not linked to WhatsApp; pair it again", "account", a.Name)
			continue
		}
		jid, err := types.ParseJID(a.JID)
		if err != nil {
			m.log.Error("account has an invalid JID", "account", a.Name, "jid", a.JID)
			continue
		}
		device, err := m.container.GetDevice(ctx, jid)
		if err != nil {
			return fmt.Errorf("load device of account %s: %w", a.Name, err)
		}
		if device == nil {
			m.log.Warn("account lost its WhatsApp session; pair it again", "account", a.Name)
			_ = m.store.SetAccountJID(a.Name, "")
			continue
		}

		client := m.newClient(a.Name, device)
		m.setClient(a.Name, client)
		go m.connect(a.Name, client)
	}
	return nil
}

// connect retries the first connection; after that whatsmeow reconnects by
// itself.
func (m *Manager) connect(account string, client *whatsmeow.Client) {
	for delay := 2 * time.Second; ; delay = min(delay*2, time.Minute) {
		if m.Client(account) != client {
			return // removed or logged out meanwhile
		}
		err := client.Connect()
		if err == nil || errors.Is(err, whatsmeow.ErrAlreadyConnected) {
			return
		}
		m.log.Warn("could not connect to WhatsApp, retrying", "account", account, "err", err, "in", delay.String())
		time.Sleep(delay)
	}
}

// Stop disconnects every client and closes the session database.
func (m *Manager) Stop() {
	m.mu.Lock()
	clients := m.clients
	m.clients = map[string]*whatsmeow.Client{}
	m.mu.Unlock()

	for _, c := range clients {
		c.Disconnect()
	}
	m.db.Close()
}

func (m *Manager) newClient(account string, device *store.Device) *whatsmeow.Client {
	client := whatsmeow.NewClient(device, waLog.Stdout("wa/"+account, "WARN", false))
	client.AddEventHandler(func(evt any) {
		defer func() {
			if r := recover(); r != nil {
				m.log.Error("panic while handling WhatsApp event", "account", account, "panic", r)
			}
		}()

		switch e := evt.(type) {
		case *events.Message:
			m.captureMessage(client, account, e, true)
		case *events.HistorySync:
			m.captureHistory(client, account, e)
		case *events.Connected:
			m.log.Info("connected to WhatsApp", "account", account)
		case *events.LoggedOut:
			m.log.Warn("WhatsApp logged this device out; pair the account again", "account", account, "reason", e.Reason.String())
			m.dropClient(account, client)
			_ = m.store.SetAccountJID(account, "")
		}
	})
	return client
}

// Client returns the client of an account, or nil.
func (m *Manager) Client(account string) *whatsmeow.Client {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.clients[account]
}

func (m *Manager) setClient(account string, client *whatsmeow.Client) {
	m.mu.Lock()
	m.clients[account] = client
	m.mu.Unlock()
}

func (m *Manager) dropClient(account string, client *whatsmeow.Client) {
	m.mu.Lock()
	if m.clients[account] == client {
		delete(m.clients, account)
	}
	m.mu.Unlock()
	go client.Disconnect()
}

// ready returns the account's client if it can be used right now, or an error
// worded for the assistant's user.
func (m *Manager) ready(account string) (*whatsmeow.Client, error) {
	client := m.Client(account)
	if client == nil || !client.IsLoggedIn() {
		if client != nil && client.Store.ID != nil && !client.IsConnected() {
			return nil, errors.New("not connected to WhatsApp right now; try again in a moment")
		}
		return nil, errors.New("this account is not linked to WhatsApp; it has to be paired again on the server")
	}
	if !client.IsConnected() {
		return nil, errors.New("not connected to WhatsApp right now; try again in a moment")
	}
	return client, nil
}

// ------------------------------------------------------------- accounts

// ValidAccountName reports whether name can be used as an account name.
func ValidAccountName(name string) bool { return accountName.MatchString(name) }

// HashToken is how account tokens are stored.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func newToken() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return tokenPrefix + hex.EncodeToString(b)
}

// AccountByToken resolves an account token to the account name.
func (m *Manager) AccountByToken(token string) (string, bool) {
	if token == "" {
		return "", false
	}
	a, err := m.store.AccountByTokenHash(HashToken(token))
	if err != nil || a == nil {
		return "", false
	}
	return a.Name, true
}

func (m *Manager) AccountExists(name string) bool {
	a, err := m.store.Account(name)
	return err == nil && a != nil
}

// RotateToken replaces the token of an existing account and returns the new one.
func (m *Manager) RotateToken(name string) (string, error) {
	a, err := m.store.Account(name)
	if err != nil {
		return "", err
	}
	if a == nil {
		return "", fmt.Errorf("account %q does not exist", name)
	}
	token := newToken()
	if err := m.store.PutAccount(name, HashToken(token)); err != nil {
		return "", err
	}
	return token, nil
}

// Remove unlinks the device from WhatsApp and deletes the account with all
// its stored messages.
func (m *Manager) Remove(ctx context.Context, name string) error {
	a, err := m.store.Account(name)
	if err != nil {
		return err
	}
	if a == nil {
		return fmt.Errorf("account %q does not exist", name)
	}

	if client := m.Client(name); client != nil {
		m.mu.Lock()
		delete(m.clients, name)
		m.mu.Unlock()

		if client.IsLoggedIn() {
			// Logout also deletes the device from the session database.
			if err := client.Logout(ctx); err != nil {
				m.log.Warn("could not log out from WhatsApp; removing the local session only", "account", name, "err", err)
				client.Disconnect()
				_ = client.Store.Delete(ctx)
			}
		} else {
			client.Disconnect()
			_ = client.Store.Delete(ctx)
		}
	}

	if err := m.store.DeleteAccount(name); err != nil {
		return err
	}
	if m.OnRemove != nil {
		m.OnRemove(name)
	}
	return nil
}

// AccountStatus describes one account for the admin CLI and get_status.
type AccountStatus struct {
	Account   string `json:"account"`
	Connected bool   `json:"connected"`
	LoggedIn  bool   `json:"logged_in"`
	JID       string `json:"jid,omitempty"`
	Name      string `json:"name,omitempty"`
}

func (m *Manager) Status(account string) AccountStatus {
	st := AccountStatus{Account: account}
	client := m.Client(account)
	if client == nil {
		return st
	}
	st.Connected = client.IsConnected()
	st.LoggedIn = client.IsLoggedIn()
	if id := client.Store.ID; id != nil {
		st.JID = id.ToNonAD().String()
		st.Name = client.Store.PushName
	}
	return st
}

func (m *Manager) Statuses() ([]AccountStatus, error) {
	accounts, err := m.store.Accounts()
	if err != nil {
		return nil, err
	}
	out := make([]AccountStatus, 0, len(accounts))
	for _, a := range accounts {
		out = append(out, m.Status(a.Name))
	}
	return out, nil
}

// ------------------------------------------------------------- pairing

// PairEvent is one step of a pairing attempt.
type PairEvent struct {
	// Type is "qr" (Code is the QR payload to render), "code" (Code is the
	// 8-character code to type on the phone) or "success".
	Type string `json:"type"`
	Code string `json:"code,omitempty"`
	JID  string `json:"jid,omitempty"`
	// Token is set on success when the account is new; it is shown only once.
	Token string `json:"token,omitempty"`
}

// Pair links a WhatsApp account. With phone empty it emits QR codes to scan;
// otherwise it emits a pairing code to type on that phone. It blocks until
// the device is linked, the attempt times out or ctx is cancelled.
func (m *Manager) Pair(ctx context.Context, name, phone string, emit func(PairEvent)) error {
	if !ValidAccountName(name) {
		return errors.New("account names use lowercase letters, digits, '-' and '_' (max 32 characters)")
	}

	m.pairing.Lock()
	defer m.pairing.Unlock()

	existing, err := m.store.Account(name)
	if err != nil {
		return err
	}
	if existing != nil && existing.JID != "" {
		return fmt.Errorf("account %q is already linked (%s); remove it first to link another number", name, existing.JID)
	}

	device := m.container.NewDevice()
	client := m.newClient(name, device)

	qr, err := client.GetQRChannel(ctx)
	if err != nil {
		return err
	}
	if err := client.Connect(); err != nil {
		return fmt.Errorf("connect to WhatsApp: %w", err)
	}

	linked := false
	codeSent := false
	for item := range qr {
		switch item.Event {
		case "code":
			if phone == "" {
				emit(PairEvent{Type: "qr", Code: item.Code})
			} else if !codeSent {
				code, err := client.PairPhone(ctx, phone, true, whatsmeow.PairClientChrome, "Chrome (Linux)")
				if err != nil {
					client.Disconnect()
					return fmt.Errorf("request pairing code: %w", err)
				}
				codeSent = true
				emit(PairEvent{Type: "code", Code: code})
			}
		case "success":
			linked = true
		case "timeout":
			client.Disconnect()
			return errors.New("pairing timed out; run it again")
		default:
			if item.Error != nil {
				client.Disconnect()
				return fmt.Errorf("pairing failed: %w", item.Error)
			}
		}
	}

	if !linked || client.Store.ID == nil {
		client.Disconnect()
		if err := ctx.Err(); err != nil {
			return err
		}
		return errors.New("pairing did not complete")
	}

	result := PairEvent{Type: "success", JID: client.Store.ID.ToNonAD().String()}
	if existing == nil {
		result.Token = newToken()
		if err := m.store.PutAccount(name, HashToken(result.Token)); err != nil {
			return err
		}
	}
	if err := m.store.SetAccountJID(name, client.Store.ID.String()); err != nil {
		return err
	}
	m.setClient(name, client)

	m.log.Info("account linked", "account", name, "jid", result.JID)
	emit(result)
	return nil
}
