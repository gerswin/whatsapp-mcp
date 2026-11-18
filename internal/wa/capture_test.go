package wa

import (
	"crypto/sha256"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waWeb"
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
		"broadcast list": {Info: types.MessageInfo{ID: "1", MessageSource: types.MessageSource{Chat: types.NewJID("123", types.BroadcastServer)}}, Message: text},
		"newsletter":     {Info: types.MessageInfo{ID: "1", MessageSource: types.MessageSource{Chat: types.NewJID("1", types.NewsletterServer)}}, Message: text},
		"reaction":       {Info: types.MessageInfo{ID: "1", MessageSource: types.MessageSource{Chat: chat}}, Message: &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{Text: proto.String("👍")}}},
		"missing id":     {Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: chat}}, Message: text},
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

func TestCaptureReactionsAndPollVotes(t *testing.T) {
	mgr := newManager(t)
	s := mgr.store

	group := types.NewJID("120363000000000001", types.GroupServer)
	ana := types.NewJID("573001111111", types.DefaultUserServer)
	luis := types.NewJID("573002222222", types.DefaultUserServer)
	from := func(sender types.JID, id string, minute int) types.MessageInfo {
		return types.MessageInfo{
			MessageSource: types.MessageSource{Chat: group, Sender: sender, IsGroup: true},
			ID:            id, Timestamp: at(minute),
		}
	}
	target := func(id string) *waCommon.MessageKey { return &waCommon.MessageKey{ID: proto.String(id)} }

	mgr.captureMessage(nil, "inst", &events.Message{Info: from(ana, "p1", 1), Message: &waE2E.Message{
		PollCreationMessage: &waE2E.PollCreationMessage{
			Name: proto.String("¿Qué día?"),
			Options: []*waE2E.PollCreationMessage_Option{
				{OptionName: proto.String("Lunes")}, {OptionName: proto.String("Martes")},
			},
		},
	}}, true)

	react := func(sender types.JID, id, emoji string, minute int) {
		mgr.captureMessage(nil, "inst", &events.Message{Info: from(sender, id, minute), Message: &waE2E.Message{
			ReactionMessage: &waE2E.ReactionMessage{Key: target("p1"), Text: proto.String(emoji)},
		}}, true)
	}
	react(ana, "r1", "👍", 2)
	react(luis, "r2", "🔥", 3)
	react(ana, "r3", "", 4) // removed

	// Votes carry the SHA-256 of the chosen option names.
	hash := func(option string) []byte { h := sha256.Sum256([]byte(option)); return h[:] }
	mgr.savePollVote("inst", group.String(), "p1", luis.String(), false, [][]byte{hash("Martes"), hash("not an option")}, at(5))
	mgr.savePollVote("inst", group.String(), "missing", luis.String(), false, [][]byte{hash("Martes")}, at(5))

	// History sync delivers reactions and votes attached to the message.
	poll, _ := s.Get("inst", group.String(), "p1")
	mgr.captureHistoryExtras(nil, "inst", *poll, &waWeb.WebMessageInfo{
		Reactions: []*waWeb.Reaction{{
			Key:  &waCommon.MessageKey{Participant: proto.String(ana.String())},
			Text: proto.String("😮"), SenderTimestampMS: proto.Int64(at(6).UnixMilli()),
		}},
		PollUpdates: []*waWeb.PollUpdate{{
			PollUpdateMessageKey: &waCommon.MessageKey{FromMe: proto.Bool(true)},
			Vote:                 &waE2E.PollVoteMessage{SelectedOptions: [][]byte{hash("Lunes")}},
			SenderTimestampMS:    proto.Int64(at(7).UnixMilli()),
		}},
	})

	msgs, _ := s.Messages("inst", group.String(), time.Time{}, 10)
	if len(msgs) != 1 {
		t.Fatalf("reactions and votes must not be stored as messages: %+v", msgs)
	}
	if err := s.Annotate("inst", msgs); err != nil {
		t.Fatalf("Annotate: %v", err)
	}
	got := msgs[0]

	if len(got.Reactions) != 2 || got.Reactions[0].SenderJID != luis.String() || got.Reactions[0].Emoji != "🔥" ||
		got.Reactions[1].SenderJID != ana.String() || got.Reactions[1].Emoji != "😮" {
		t.Fatalf("unexpected reactions: %+v", got.Reactions)
	}
	if len(got.Votes) != 2 || got.Votes[0].VoterJID != luis.String() || len(got.Votes[0].Options) != 1 || got.Votes[0].Options[0] != "Martes" ||
		!got.Votes[1].FromMe || got.Votes[1].Options[0] != "Lunes" {
		t.Fatalf("unexpected votes: %+v", got.Votes)
	}
}

func TestStatusUpdatesAreOptIn(t *testing.T) {
	ana := types.NewJID("573001111111", types.DefaultUserServer)
	status := &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: types.StatusBroadcastJID, Sender: ana},
			ID:            "s1", PushName: "Ana", Timestamp: at(1),
		},
		Message: &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("de viaje")}},
	}
	chat := types.StatusBroadcastJID.String()

	off := newManager(t)
	off.captureMessage(nil, "inst", status, true)
	if m, _ := off.store.Get("inst", chat, "s1"); m != nil {
		t.Fatalf("status stored although STORE_STATUS is off: %+v", m)
	}

	on := newManager(t)
	on.storeStatus = true
	on.captureMessage(nil, "inst", status, true)
	m, _ := on.store.Get("inst", chat, "s1")
	if m == nil || m.Text != "de viaje" || m.SenderJID != ana.String() || m.SenderName != "Ana" {
		t.Fatalf("status not stored: %+v", m)
	}
	// All statuses share one chat; it must not take the name of a poster.
	if name := on.store.ChatName("inst", chat); name != statusChatName {
		t.Fatalf("status chat name = %q", name)
	}
}

func TestSendingIsLimitedToPeopleAndGroups(t *testing.T) {
	mgr := newManager(t)
	for _, to := range []string{"status@broadcast", "120363000000000009@newsletter", "123@broadcast"} {
		if _, err := mgr.SendText(t.Context(), "inst", to, "hola", ""); err == nil || !strings.Contains(err.Error(), "only be sent to people and groups") {
			t.Errorf("SendText(%q) = %v, want a refusal", to, err)
		}
	}
}
