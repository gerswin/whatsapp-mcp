package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gerswin/whatsapp-mcp/internal/store"
	"github.com/gerswin/whatsapp-mcp/internal/wa"
)

const (
	tokenA = "token-a"
	tokenB = "token-b"
	alice  = "5511911112222@s.whatsapp.net"
)

// fakeBackend stands in for WhatsApp. Embedding the interface means only the
// methods under test need bodies; anything else panics loudly if reached.
type fakeBackend struct {
	Backend
	texts []sentText
}

type sentText struct{ account, to, text, replyTo string }

func (*fakeBackend) AccountByToken(token string) (string, bool) {
	switch token {
	case tokenA:
		return "inst-a", true
	case tokenB:
		return "inst-b", true
	}
	return "", false
}

func (*fakeBackend) AccountExists(name string) bool { return name == "inst-a" || name == "inst-b" }

func (*fakeBackend) Status(account string) wa.AccountStatus {
	return wa.AccountStatus{Account: account, Connected: true, Name: "Ventas WA"}
}

func (*fakeBackend) ContactName(context.Context, string, string) string { return "" }

func (f *fakeBackend) SendText(_ context.Context, account, to, text, replyTo string) (wa.Sent, error) {
	f.texts = append(f.texts, sentText{account, to, text, replyTo})
	return wa.Sent{MessageID: "SENT1", Chat: alice, Timestamp: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}, nil
}

func (*fakeBackend) Media(_ context.Context, account, chat, id string) (*wa.Media, error) {
	switch id {
	case "img":
		return &wa.Media{Kind: "image", Image: []byte{0xFF, 0xD8, 0xFF}}, nil
	case "voice":
		return &wa.Media{Kind: "voice_note", Text: "llego en diez minutos"}, nil
	case "silent":
		return &wa.Media{Kind: "voice_note"}, nil
	}
	return nil, errors.New("that message is not in the stored history of this chat")
}

type headerTransport struct {
	header, value string
}

func (h headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set(h.header, h.value)
	return http.DefaultTransport.RoundTrip(req)
}

// revokeAccount is the function returned by the last Register call.
var revokeAccount func(string) error

func startServer(t *testing.T, st *store.Store) (*httptest.Server, *fakeBackend) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	if st == nil {
		var err error
		if st, err = store.Open(filepath.Join(t.TempDir(), "empty.db")); err != nil {
			t.Fatalf("open store: %v", err)
		}
		t.Cleanup(func() { st.Close() })
	}

	backend := &fakeBackend{}
	r := gin.New()
	var err error
	if revokeAccount, err = Register(r, Config{
		Backend:       backend,
		Accounts:      backend,
		Store:         st,
		OAuthDBPath:   filepath.Join(t.TempDir(), "oauth.db"),
		Version:       "test",
		Transcription: true,
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, backend
}

func connect(t *testing.T, srv *httptest.Server, header, value string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:             srv.URL + Path,
		HTTPClient:           &http.Client{Transport: headerTransport{header, value}},
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

// call invokes a tool and decodes its structured result into out.
func call(t *testing.T, session *mcp.ClientSession, name string, args any, out any) *mcp.CallToolResult {
	t.Helper()
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: protocol error: %v", name, err)
	}
	if out != nil {
		if res.IsError {
			t.Fatalf("%s: tool error: %+v", name, res.Content)
		}
		raw, _ := json.Marshal(res.StructuredContent)
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("%s: decode %s: %v", name, raw, err)
		}
	}
	return res
}

func seededStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	if err := st.Save("inst-a",
		store.Message{ID: "m1", ChatJID: alice, SenderJID: alice, SenderName: "Alice", Type: "text", Text: "¿Tienen stock del modelo X?", Timestamp: base},
		store.Message{ID: "m2", ChatJID: alice, FromMe: true, Type: "text", Text: "Sí, quedan 3", Timestamp: base.Add(time.Minute)},
		store.Message{ID: "img", ChatJID: alice, SenderJID: alice, Type: "image", Timestamp: base.Add(-time.Minute), Media: []byte{1}},
	); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := st.Save("inst-b", store.Message{ID: "s1", ChatJID: alice, SenderJID: alice, Type: "text", Text: "stock privado de otra cuenta", Timestamp: base}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := st.SetChatName("inst-a", alice, "Alice"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return st
}

func TestRejectsMissingOrInvalidToken(t *testing.T) {
	srv, _ := startServer(t, nil)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`

	for name, header := range map[string][2]string{
		"no token":      {"X-Nothing", "x"},
		"unknown token": {"apikey", "nope"},
		"basic scheme":  {"Authorization", "Basic " + tokenA},
		"fake oauth":    {"Authorization", "Bearer " + accessTokenPrefix + "made-up"},
	} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+Path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set(header[0], header[1])
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", name, resp.StatusCode)
		}
		if resp.Header.Get("WWW-Authenticate") == "" {
			t.Errorf("%s: missing WWW-Authenticate header", name)
		}
	}
}

func TestHistoryToolsAreScopedToTheAccount(t *testing.T) {
	srv, _ := startServer(t, seededStore(t))
	session := connect(t, srv, "apikey", tokenA)

	var status statusOutput
	call(t, session, "get_status", map[string]any{}, &status)
	if status.Account != "inst-a" || !status.Connected || status.LoggedIn || status.Name != "Ventas WA" {
		t.Fatalf("unexpected status: %+v", status)
	}

	var chats listChatsOutput
	call(t, session, "list_chats", map[string]any{}, &chats)
	if len(chats.Chats) != 1 {
		t.Fatalf("want 1 chat, got %+v", chats.Chats)
	}
	if c := chats.Chats[0]; c.JID != alice || c.Name != "Alice" || c.IsGroup || c.MessageCount != 3 || c.LastMessage != "You: Sí, quedan 3" {
		t.Fatalf("unexpected chat: %+v", c)
	}

	// A bare phone number must resolve to the same chat as its JID.
	var msgs getMessagesOutput
	call(t, session, "get_messages", map[string]any{"chat": "+55 11 91111-2222"}, &msgs)
	if len(msgs.Messages) != 3 || msgs.Messages[0].ID != "img" || msgs.Messages[1].ID != "m1" || msgs.Messages[2].ID != "m2" {
		t.Fatalf("unexpected messages: %+v", msgs.Messages)
	}
	if m := msgs.Messages[0]; !m.HasMedia || m.Type != "image" {
		t.Fatalf("image should be flagged as openable: %+v", m)
	}
	if m := msgs.Messages[1]; m.Sender != alice || m.SenderName != "Alice" || m.FromMe || m.HasMedia {
		t.Fatalf("unexpected incoming message: %+v", m)
	}
	if m := msgs.Messages[2]; !m.FromMe || m.Sender != "" {
		t.Fatalf("unexpected outgoing message: %+v", m)
	}

	var found searchMessagesOutput
	call(t, session, "search_messages", map[string]any{"query": "STOCK"}, &found)
	if len(found.Messages) != 1 || found.Messages[0].ID != "m1" || found.Messages[0].Chat != alice || found.Messages[0].ChatName != "Alice" {
		t.Fatalf("search leaked or missed messages: %+v", found.Messages)
	}

	// The other token sees only its own account's data.
	other := connect(t, srv, "Authorization", "Bearer "+tokenB)
	call(t, other, "search_messages", map[string]any{"query": "stock"}, &found)
	if len(found.Messages) != 1 || found.Messages[0].ID != "s1" {
		t.Fatalf("account B got: %+v", found.Messages)
	}
}

func TestSendTextAndInputValidation(t *testing.T) {
	srv, backend := startServer(t, seededStore(t))
	session := connect(t, srv, "apikey", tokenA)

	var out sentOutput
	call(t, session, "send_text", map[string]any{"to": "5511911112222", "text": "Hola", "reply_to": "m1"}, &out)
	if out.MessageID != "SENT1" || out.Chat != alice || out.Timestamp != "2026-01-01T12:00:00Z" {
		t.Fatalf("unexpected send result: %+v", out)
	}
	if len(backend.texts) != 1 {
		t.Fatalf("SendText called %d times", len(backend.texts))
	}
	if got := backend.texts[0]; got != (sentText{"inst-a", "5511911112222", "Hola", "m1"}) {
		t.Fatalf("unexpected SendText call: %+v", got)
	}

	// Handler-level validation surfaces as a tool error, and nothing is sent.
	res := call(t, session, "send_text", map[string]any{"to": "5511911112222", "text": "   "}, nil)
	if !res.IsError {
		t.Fatalf("blank text should be a tool error")
	}
	// Schema-level validation: missing required argument.
	if res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "send_text", Arguments: map[string]any{"text": "x"}}); err == nil && !res.IsError {
		t.Fatalf("missing 'to' should be rejected")
	}
	if len(backend.texts) != 1 {
		t.Fatalf("invalid calls reached SendText (%d calls)", len(backend.texts))
	}
}

func TestGetMedia(t *testing.T) {
	srv, _ := startServer(t, seededStore(t))
	session := connect(t, srv, "apikey", tokenA)

	// Images come back as an image block the assistant can look at.
	res := call(t, session, "get_media", map[string]any{"chat": alice, "message_id": "img"}, nil)
	if res.IsError || len(res.Content) != 2 {
		t.Fatalf("unexpected image result: %+v", res)
	}
	img, ok := res.Content[0].(*mcp.ImageContent)
	if !ok || img.MIMEType != "image/jpeg" || string(img.Data) != "\xFF\xD8\xFF" {
		t.Fatalf("first block should be the JPEG: %+v", res.Content[0])
	}

	// Audio comes back as its transcript.
	res = call(t, session, "get_media", map[string]any{"chat": alice, "message_id": "voice"}, nil)
	text, ok := res.Content[0].(*mcp.TextContent)
	if res.IsError || !ok || !strings.Contains(text.Text, "llego en diez minutos") || !strings.Contains(text.Text, "voice_note") {
		t.Fatalf("unexpected transcript result: %+v", res.Content)
	}

	res = call(t, session, "get_media", map[string]any{"chat": alice, "message_id": "silent"}, nil)
	if text, ok := res.Content[0].(*mcp.TextContent); res.IsError || !ok || !strings.Contains(text.Text, "No speech") {
		t.Fatalf("empty transcript should be explained: %+v", res.Content)
	}

	// Backend failures are tool errors the assistant can read, not protocol errors.
	res = call(t, session, "get_media", map[string]any{"chat": alice, "message_id": "missing"}, nil)
	if !res.IsError {
		t.Fatalf("unknown message should be a tool error")
	}
}

func TestToolList(t *testing.T) {
	srv, _ := startServer(t, nil)
	session := connect(t, srv, "apikey", tokenA)
	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	tools := map[string]*mcp.Tool{}
	for _, tool := range res.Tools {
		tools[tool.Name] = tool
	}

	want := []string{
		"get_status", "list_chats", "get_messages", "search_messages", "get_media",
		"list_contacts", "check_numbers", "list_groups", "get_group_info",
		"send_text", "send_media", "send_location", "send_poll", "react_to_message", "mark_read",
	}
	for _, name := range want {
		if _, ok := tools[name]; !ok {
			t.Errorf("tool %s is missing", name)
		}
	}
	if len(tools) != len(want) {
		t.Errorf("expected %d tools, got %d", len(want), len(tools))
	}

	for _, name := range []string{"get_messages", "get_media"} {
		if a := tools[name].Annotations; a == nil || !a.ReadOnlyHint {
			t.Errorf("%s should be annotated read-only: %+v", name, a)
		}
	}
	if a := tools["send_text"].Annotations; a == nil || a.ReadOnlyHint || a.DestructiveHint == nil || *a.DestructiveHint {
		t.Errorf("send_text should be a non-destructive write: %+v", a)
	}
}
