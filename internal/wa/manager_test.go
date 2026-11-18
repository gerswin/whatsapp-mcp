package wa

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"go.mau.fi/whatsmeow/proto/waAdv"
	"go.mau.fi/whatsmeow/types"

	"github.com/gerswin/whatsapp-mcp/internal/store"
)

func startManager(t *testing.T) *Manager {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "messages.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	m, err := NewManager(context.Background(), Options{DataDir: dir, Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(func() {
		m.Stop()
		st.Close()
	})
	return m
}

// The WhatsApp session database runs on the pure-Go SQLite driver. This
// exercises its migrations and the queries the read tools depend on, without
// talking to WhatsApp.
func TestSessionDatabaseOnSQLite(t *testing.T) {
	m := startManager(t)
	ctx := context.Background()

	jid := types.NewADJID("573001234567", 0, 7)
	device := m.container.NewDevice()
	device.ID = &jid
	// A real pairing fills this in; the database requires it.
	device.Account = &waAdv.ADVSignedDeviceIdentity{
		Details:             []byte("details"),
		AccountSignature:    make([]byte, 64),
		AccountSignatureKey: make([]byte, 32),
		DeviceSignature:     make([]byte, 64),
	}
	if err := device.Save(ctx); err != nil {
		t.Fatalf("save device: %v", err)
	}

	loaded, err := m.container.GetDevice(ctx, jid)
	if err != nil || loaded == nil {
		t.Fatalf("device not found after saving (err %v)", err)
	}

	friend := types.NewJID("573009998877", types.DefaultUserServer)
	if _, _, err := loaded.Contacts.PutPushName(ctx, friend, "Carla"); err != nil {
		t.Fatalf("PutPushName: %v", err)
	}
	if err := loaded.Contacts.PutContactName(ctx, friend, "Carla", "Carla Pérez"); err != nil {
		t.Fatalf("PutContactName: %v", err)
	}
	lid := types.NewJID("123456789", types.HiddenUserServer)
	if err := loaded.LIDs.PutLIDMapping(ctx, lid, friend); err != nil {
		t.Fatalf("PutLIDMapping: %v", err)
	}
	if pn, err := loaded.LIDs.GetPNForLID(ctx, lid); err != nil || pn.User != friend.User {
		t.Fatalf("GetPNForLID = %v, %v", pn, err)
	}

	// Wire the device in as a (never connected) client and read through the
	// same paths the tools use.
	m.setClient("co", m.newClient("co", loaded))

	contacts, err := m.Contacts(ctx, "co")
	if err != nil {
		t.Fatalf("Contacts: %v", err)
	}
	if len(contacts) != 1 || contacts[0].JID != friend.String() || contacts[0].Name != "Carla Pérez" || contacts[0].PushName != "Carla" {
		t.Fatalf("unexpected contacts: %+v", contacts)
	}
	if name := m.ContactName(ctx, "co", friend.String()); name != "Carla Pérez" {
		t.Fatalf("ContactName = %q", name)
	}

	st := m.Status("co")
	if st.Connected || st.LoggedIn || st.JID != "573001234567@s.whatsapp.net" {
		t.Fatalf("unexpected status: %+v", st)
	}
	// Not connected: sending must fail with a readable error, not panic.
	if _, err := m.SendText(ctx, "co", friend.String(), "hola", ""); err == nil {
		t.Fatal("sending while disconnected should fail")
	}
	if _, err := m.SendText(ctx, "nope", friend.String(), "hola", ""); err == nil {
		t.Fatal("sending from an unknown account should fail")
	}
}

func TestAccountsAndTokens(t *testing.T) {
	m := startManager(t)

	if err := m.Pair(context.Background(), "Not Valid!", "", func(PairEvent) {}); err == nil {
		t.Fatal("invalid account names must be rejected before touching WhatsApp")
	}

	tok := newToken()
	if err := m.store.PutAccount("co", HashToken(tok)); err != nil {
		t.Fatal(err)
	}
	if name, ok := m.AccountByToken(tok); !ok || name != "co" {
		t.Fatalf("AccountByToken = %q, %v", name, ok)
	}
	if _, ok := m.AccountByToken(""); ok {
		t.Fatal("empty token must not match")
	}

	rotated, err := m.RotateToken("co")
	if err != nil || rotated == tok {
		t.Fatalf("RotateToken = %q, %v", rotated, err)
	}
	if _, ok := m.AccountByToken(tok); ok {
		t.Fatal("old token still works after rotation")
	}
	if name, ok := m.AccountByToken(rotated); !ok || name != "co" {
		t.Fatal("new token does not work")
	}
	if _, err := m.RotateToken("missing"); err == nil {
		t.Fatal("rotating an unknown account should fail")
	}

	removed := ""
	m.OnRemove = func(a string) { removed = a }
	_ = m.store.Save("co", store.Message{ID: "m1", ChatJID: "1@s.whatsapp.net", Type: "text", Text: "x"})
	if err := m.Remove(context.Background(), "co"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if m.AccountExists("co") || removed != "co" {
		t.Fatalf("account not fully removed (exists=%v, hook=%q)", m.AccountExists("co"), removed)
	}
	if msg, _ := m.store.Get("co", "1@s.whatsapp.net", "m1"); msg != nil {
		t.Fatal("messages survived account removal")
	}
}
