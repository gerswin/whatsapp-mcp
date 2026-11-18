package store

import (
	"path/filepath"
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "sub", "messages.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func at(minute int) time.Time {
	return time.Date(2026, 1, 1, 12, minute, 0, 0, time.UTC)
}

func TestStoreSaveListAndPage(t *testing.T) {
	s := newStore(t)
	const alice, bob = "111@s.whatsapp.net", "222@s.whatsapp.net"

	err := s.Save("inst",
		Message{ID: "a1", ChatJID: alice, SenderJID: alice, Type: "text", Text: "hola", Timestamp: at(1)},
		Message{ID: "a2", ChatJID: alice, FromMe: true, Type: "text", Text: "qué tal", Timestamp: at(2)},
		Message{ID: "a3", ChatJID: alice, SenderJID: alice, Type: "image", Text: "foto", Timestamp: at(3)},
		Message{ID: "b1", ChatJID: bob, SenderJID: bob, Type: "text", Text: "100% listo", Timestamp: at(5)},
	)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	// A replay must not duplicate or overwrite.
	if err := s.Save("inst", Message{ID: "a1", ChatJID: alice, Type: "text", Text: "OVERWRITTEN", Timestamp: at(9)}); err != nil {
		t.Fatalf("Save replay: %v", err)
	}
	// Another instance's data must stay invisible.
	if err := s.Save("other", Message{ID: "x1", ChatJID: alice, Type: "text", Text: "hola secreto", Timestamp: at(8)}); err != nil {
		t.Fatalf("Save other: %v", err)
	}
	if err := s.SetChatName("inst", alice, "Alice"); err != nil {
		t.Fatalf("SetChatName: %v", err)
	}

	chats, err := s.ListChats("inst", "", 10)
	if err != nil {
		t.Fatalf("ListChats: %v", err)
	}
	if len(chats) != 2 || chats[0].JID != bob || chats[1].JID != alice {
		t.Fatalf("chats not ordered by recency: %+v", chats)
	}
	if c := chats[1]; c.Name != "Alice" || c.MessageCount != 3 || c.LastMessageText != "foto" || c.LastMessageType != "image" || !c.LastMessageAt.Equal(at(3)) {
		t.Fatalf("unexpected alice summary: %+v", c)
	}

	if chats, _ = s.ListChats("inst", "ali", 10); len(chats) != 1 || chats[0].JID != alice {
		t.Fatalf("name filter failed: %+v", chats)
	}

	msgs, err := s.Messages("inst", alice, time.Time{}, 2)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(msgs) != 2 || msgs[0].ID != "a2" || msgs[1].ID != "a3" {
		t.Fatalf("expected latest two, oldest first: %+v", msgs)
	}
	older, _ := s.Messages("inst", alice, msgs[0].Timestamp, 2)
	if len(older) != 1 || older[0].ID != "a1" || older[0].Text != "hola" {
		t.Fatalf("paging back failed: %+v", older)
	}
}

func TestStoreSearchEditDeletePurge(t *testing.T) {
	s := newStore(t)
	const alice, bob = "111@s.whatsapp.net", "222@s.whatsapp.net"

	if err := s.Save("inst",
		Message{ID: "a1", ChatJID: alice, Type: "text", Text: "Reunión mañana", Timestamp: at(1)},
		Message{ID: "b1", ChatJID: bob, Type: "text", Text: "100% listo", Timestamp: at(2)},
		Message{ID: "b2", ChatJID: bob, Type: "text", Text: "1000 listo", Timestamp: at(3)},
	); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if got, _ := s.Search("inst", "reuni", "", 10); len(got) != 1 || got[0].ID != "a1" {
		t.Fatalf("case-insensitive search failed: %+v", got)
	}
	// "%" must match literally, not act as a wildcard.
	if got, _ := s.Search("inst", "100%", "", 10); len(got) != 1 || got[0].ID != "b1" {
		t.Fatalf("LIKE wildcard was not escaped: %+v", got)
	}
	if got, _ := s.Search("inst", "listo", alice, 10); len(got) != 0 {
		t.Fatalf("chat filter ignored: %+v", got)
	}

	if err := s.EditText("inst", alice, "a1", "Reunión pasado mañana"); err != nil {
		t.Fatalf("EditText: %v", err)
	}
	if m, _ := s.Get("inst", alice, "a1"); m == nil || m.Text != "Reunión pasado mañana" {
		t.Fatalf("edit not applied: %+v", m)
	}

	if err := s.MarkDeleted("inst", bob, "b1"); err != nil {
		t.Fatalf("MarkDeleted: %v", err)
	}
	if m, _ := s.Get("inst", bob, "b1"); m == nil || !m.Deleted || m.Text != "" {
		t.Fatalf("delete not applied: %+v", m)
	}

	if n, err := s.Prune(at(2)); err != nil || n != 1 {
		t.Fatalf("Prune removed %d (err %v), want 1", n, err)
	}

	if err := s.PurgeAccount("inst"); err != nil {
		t.Fatalf("PurgeAccount: %v", err)
	}
	if chats, _ := s.ListChats("inst", "", 10); len(chats) != 0 {
		t.Fatalf("purge left chats behind: %+v", chats)
	}
}
