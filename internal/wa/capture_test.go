package wa

import (
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/gerswin/whatsapp-mcp/internal/store"
)

// newManager returns a Manager with a real store and no WhatsApp connection,
// which is all the capture path needs.
func newManager(t *testing.T) *Manager {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return &Manager{store: s, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func at(minute int) time.Time {
	return time.Date(2026, 1, 1, 12, minute, 0, 0, time.UTC)
}

func TestCaptureMessageLifecycle(t *testing.T) {
	mgr := newManager(t)
	s := mgr.store

	lid := types.NewJID("99999", types.HiddenUserServer)
	phone := types.NewJID("5511988887777", types.DefaultUserServer)
	info := types.MessageInfo{
		MessageSource: types.MessageSource{Chat: lid, Sender: lid, SenderAlt: phone},
		ID:            "m1",
		PushName:      "Carla",
		Timestamp:     at(1),
	}

	mgr.captureMessage(nil, "inst", &events.Message{Info: info, Message: &waE2E.Message{Conversation: proto.String("primer texto")}}, true)

	// LID-addressed chats are keyed by phone number.
	m, err := s.Get("inst", phone.String(), "m1")
	if err != nil || m == nil {
		t.Fatalf("message not stored under phone JID (err %v)", err)
	}
	if m.Text != "primer texto" || m.SenderJID != phone.String() || m.SenderName != "Carla" || m.Type != "text" {
		t.Fatalf("unexpected stored message: %+v", m)
	}
	if name := s.ChatName("inst", phone.String()); name != "Carla" {
		t.Fatalf("chat name = %q, want Carla", name)
	}

	protocol := func(id string, pm *waE2E.ProtocolMessage) *events.Message {
		i := info
		i.ID = id
		pm.Key = &waCommon.MessageKey{ID: proto.String("m1")}
		return &events.Message{Info: i, Message: &waE2E.Message{ProtocolMessage: pm}}
	}

	mgr.captureMessage(nil, "inst", protocol("e1", &waE2E.ProtocolMessage{
		Type:          waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(),
		EditedMessage: &waE2E.Message{Conversation: proto.String("texto editado")},
	}), true)
	if m, _ = s.Get("inst", phone.String(), "m1"); m.Text != "texto editado" {
		t.Fatalf("edit not captured: %+v", m)
	}

	mgr.captureMessage(nil, "inst", protocol("r1", &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_REVOKE.Enum()}), true)
	if m, _ = s.Get("inst", phone.String(), "m1"); !m.Deleted || m.Text != "" {
		t.Fatalf("revoke not captured: %+v", m)
	}

	// Protocol traffic itself is never stored as a message.
	if msgs, _ := s.Messages("inst", phone.String(), time.Time{}, 10); len(msgs) != 1 {
		t.Fatalf("expected only the original message, got %d", len(msgs))
	}
}

func TestConvertSkipsNonChatContent(t *testing.T) {
	text := &waE2E.Message{Conversation: proto.String("hi")}
	chat := types.NewJID("111", types.DefaultUserServer)

	cases := map[string]*events.Message{
		"status broadcast": {Info: types.MessageInfo{ID: "1", MessageSource: types.MessageSource{Chat: types.StatusBroadcastJID}}, Message: text},
		"newsletter":       {Info: types.MessageInfo{ID: "1", MessageSource: types.MessageSource{Chat: types.NewJID("1", types.NewsletterServer)}}, Message: text},
		"reaction":         {Info: types.MessageInfo{ID: "1", MessageSource: types.MessageSource{Chat: chat}}, Message: &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{Text: proto.String("👍")}}},
		"missing id":       {Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: chat}}, Message: text},
	}
	for name, evt := range cases {
		if _, ok := convert(nil, evt); ok {
			t.Errorf("%s should not be stored", name)
		}
	}

	m, ok := convert(nil, &events.Message{
		Info: types.MessageInfo{ID: "d1", MessageSource: types.MessageSource{Chat: chat, Sender: chat}, Timestamp: at(1)},
		Message: &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
			FileName:    proto.String("factura.pdf"),
			MediaKey:    []byte("key"),
			DirectPath:  proto.String("/v/doc"),
			Caption:     proto.String("la de enero"),
			ContextInfo: &waE2E.ContextInfo{StanzaID: proto.String("q9")},
		}},
	})
	if !ok || m.Type != "document" || m.Text != "factura.pdf — la de enero" || m.QuotedID != "q9" {
		t.Fatalf("unexpected document conversion: ok=%v %+v", ok, m)
	}

	// The media reference keeps what is needed to download, not the context.
	var ref waE2E.Message
	if err := proto.Unmarshal(m.Media, &ref); err != nil {
		t.Fatalf("media reference: %v", err)
	}
	if d := ref.GetDocumentMessage(); d.GetDirectPath() != "/v/doc" || string(d.GetMediaKey()) != "key" || d.GetContextInfo() != nil {
		t.Fatalf("unexpected media reference: %+v", d)
	}
	if text, ok := convert(nil, &events.Message{
		Info:    types.MessageInfo{ID: "t1", MessageSource: types.MessageSource{Chat: chat, Sender: chat}},
		Message: &waE2E.Message{Conversation: proto.String("hi")},
	}); !ok || text.Media != nil {
		t.Fatalf("text messages must not carry a media reference: %+v", text)
	}
}

func TestParseRecipient(t *testing.T) {
	good := map[string]string{
		"573001234567":                   "573001234567@s.whatsapp.net",
		"+57 300 123-4567":               "573001234567@s.whatsapp.net",
		"573001234567@s.whatsapp.net":    "573001234567@s.whatsapp.net",
		"573001234567:12@s.whatsapp.net": "573001234567@s.whatsapp.net",
		"120363041234567890@g.us":        "120363041234567890@g.us",
	}
	for in, want := range good {
		jid, err := ParseRecipient(in)
		if err != nil || jid.String() != want {
			t.Errorf("ParseRecipient(%q) = %q, %v; want %q", in, jid.String(), err, want)
		}
	}
	for _, in := range []string{"", "juan", "123", "@g.us", "1234567890123456789"} {
		if jid, err := ParseRecipient(in); err == nil {
			t.Errorf("ParseRecipient(%q) = %q, want an error", in, jid.String())
		}
	}
}

func TestIsPublicIP(t *testing.T) {
	for ip, want := range map[string]bool{
		"8.8.8.8": true, "2606:4700::1111": true,
		"127.0.0.1": false, "10.1.2.3": false, "192.168.5.57": false, "172.16.0.1": false,
		"169.254.169.254": false, "100.64.1.1": false, "::1": false, "fd00::1": false, "0.0.0.0": false,
	} {
		if got := isPublicIP(net.ParseIP(ip)); got != want {
			t.Errorf("isPublicIP(%s) = %v, want %v", ip, got, want)
		}
	}
}
