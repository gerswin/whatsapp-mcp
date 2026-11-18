package wa

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/gerswin/whatsapp-mcp/internal/store"
)

// captureMessage persists one message event. live is false for messages that
// are being replayed (history), which are never transcribed automatically.
func (m *Manager) captureMessage(client *whatsmeow.Client, account string, evt *events.Message, live bool) {
	if evt == nil || evt.Message == nil {
		return
	}
	s := m.store

	status := isStatus(evt.Info.Chat)
	if status && !m.storeStatus {
		return
	}

	// Edits and "delete for everyone" arrive as protocol messages that point at
	// the original message instead of carrying a new one.
	if pm := evt.Message.GetProtocolMessage(); pm != nil {
		chat := resolveChat(client, &evt.Info)
		targetID := pm.GetKey().GetID()
		if chat == "" || targetID == "" {
			return
		}
		switch pm.GetType() {
		case waE2E.ProtocolMessage_REVOKE:
			if err := s.MarkDeleted(account, chat, targetID); err != nil {
				m.log.Error("failed to mark message deleted", "account", account, "id", targetID, "err", err)
			}
		case waE2E.ProtocolMessage_MESSAGE_EDIT:
			if _, text := describe(pm.GetEditedMessage()); text != "" {
				if err := s.EditText(account, chat, targetID, text); err != nil {
					m.log.Error("failed to apply message edit", "account", account, "id", targetID, "err", err)
				}
			}
		}
		return
	}

	if m.captureReaction(client, account, evt) || m.capturePollVote(client, account, evt) {
		return
	}

	msg, ok := convert(client, evt)
	if !ok {
		return
	}
	if err := s.Save(account, msg); err != nil {
		m.log.Error("failed to save message", "account", account, "id", msg.ID, "err", err)
		return
	}

	if status {
		// Everyone's status updates share one chat; do not name it after a poster.
		if s.ChatName(account, msg.ChatJID) == "" {
			_ = s.SetChatName(account, msg.ChatJID, statusChatName)
		}
	} else if evt.Info.IsGroup {
		if s.ChatName(account, msg.ChatJID) == "" && client != nil {
			// Group names are not in the message; fetch once, off the event loop.
			go m.fetchGroupName(client, account, evt.Info.Chat)
		}
	} else if !evt.Info.IsFromMe && evt.Info.PushName != "" {
		_ = s.SetChatName(account, msg.ChatJID, evt.Info.PushName)
	}

	if live && client != nil {
		if audio := evt.Message.GetAudioMessage(); audio != nil && m.shouldAutoTranscribe(audio) {
			go m.autoTranscribe(client, account, msg.ChatJID, msg.ID, evt.Message)
		}
	}
}

func (m *Manager) captureHistory(client *whatsmeow.Client, account string, evt *events.HistorySync) {
	if evt == nil || evt.Data == nil {
		return
	}
	s := m.store

	total := 0
	for _, conv := range evt.Data.GetConversations() {
		chatJID, err := types.ParseJID(conv.GetID())
		if err != nil || (isStatus(chatJID) && !m.storeStatus) {
			continue
		}

		var webMsgs []*waWeb.WebMessageInfo
		for _, hm := range conv.GetMessages() {
			webMsgs = append(webMsgs, hm.GetMessage())
		}
		batch := m.saveHistory(client, account, chatJID, webMsgs)
		if len(batch) == 0 {
			continue
		}
		total += len(batch)

		name := conv.GetName()
		if name == "" {
			name = conv.GetDisplayName()
		}
		if name != "" && s.ChatName(account, batch[0].ChatJID) == "" {
			_ = s.SetChatName(account, batch[0].ChatJID, name)
		}
	}

	if m.storeStatus {
		if batch := m.saveHistory(client, account, types.StatusBroadcastJID, evt.Data.GetStatusV3Messages()); len(batch) > 0 {
			total += len(batch)
			_ = s.SetChatName(account, batch[0].ChatJID, statusChatName)
		}
	}

	if total > 0 {
		m.log.Info("saved messages from history sync", "account", account, "count", total)
	}
}

// saveHistory stores the synced messages of one chat, with the reactions and
// poll votes attached to them, and returns what was stored.
func (m *Manager) saveHistory(client *whatsmeow.Client, account string, chatJID types.JID, webMsgs []*waWeb.WebMessageInfo) []store.Message {
	var batch []store.Message
	var sources []*waWeb.WebMessageInfo
	for _, web := range webMsgs {
		parsed, err := client.ParseWebMessage(chatJID, web)
		if err != nil || parsed == nil || parsed.Message == nil {
			continue
		}
		if msg, ok := convert(client, parsed); ok {
			batch = append(batch, msg)
			sources = append(sources, web)
		}
	}
	if len(batch) == 0 {
		return nil
	}

	if err := m.store.Save(account, batch...); err != nil {
		m.log.Error("failed to save history", "account", account, "chat", chatJID.String(), "err", err)
		return nil
	}
	for i, msg := range batch {
		m.captureHistoryExtras(client, account, msg, sources[i])
	}
	return batch
}

func (m *Manager) fetchGroupName(client *whatsmeow.Client, account string, group types.JID) {
	defer func() { _ = recover() }()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	info, err := client.GetGroupInfo(ctx, group)
	if err != nil || info == nil {
		return
	}
	_ = m.store.SetChatName(account, group.ToNonAD().String(), info.Name)
}

// recordSent persists a message we just sent. WhatsApp does not echo our own
// sends back as events, so the send path has to report them.
func (m *Manager) recordSent(client *whatsmeow.Client, account string, chat types.JID, id types.MessageID, ts time.Time, msg *waE2E.Message) {
	info := types.MessageInfo{
		MessageSource: types.MessageSource{
			Chat:     chat,
			Sender:   client.Store.GetJID().ToNonAD(),
			IsFromMe: true,
			IsGroup:  chat.Server == types.GroupServer,
		},
		ID:        id,
		Timestamp: ts,
	}
	stored, ok := convert(client, &events.Message{Info: info, Message: msg})
	if !ok {
		return
	}
	if err := m.store.Save(account, stored); err != nil {
		m.log.Error("failed to save sent message", "account", account, "id", id, "err", err)
	}
}

// convert turns a WhatsApp message event into a storable Message. ok is false
// for things that are not messages of a chat: broadcast lists, channels (read
// on demand, see ChannelMessages), reactions and poll votes (stored on their
// own) and other content-less protocol traffic. Status updates are converted;
// whether they are kept is the caller's decision.
func convert(client *whatsmeow.Client, evt *events.Message) (store.Message, bool) {
	info := &evt.Info
	if info.ID == "" {
		return store.Message{}, false
	}
	if (info.Chat.Server == types.BroadcastServer && !isStatus(info.Chat)) || info.Chat.Server == types.NewsletterServer {
		return store.Message{}, false
	}

	msgType, text := describe(evt.Message)
	if msgType == "" {
		return store.Message{}, false
	}

	chat := resolveChat(client, info)
	if chat == "" {
		return store.Message{}, false
	}

	ts := info.Timestamp
	if ts.IsZero() {
		ts = time.Now()
	}

	return store.Message{
		ID:          info.ID,
		ChatJID:     chat,
		SenderJID:   resolveSender(client, info),
		SenderName:  info.PushName,
		FromMe:      info.IsFromMe,
		Type:        msgType,
		Text:        text,
		QuotedID:    quotedID(evt.Message),
		Timestamp:   ts,
		Media:       mediaRef(evt.Message),
		PollOptions: pollOptions(evt.Message),
	}, true
}

// mediaRef serializes just what is needed to download a message's file later
// (keys, hashes and path), without thumbnails or quoted context. It returns
// nil for messages that carry no file.
func mediaRef(msg *waE2E.Message) []byte {
	ref := &waE2E.Message{}
	switch {
	case msg.GetImageMessage() != nil:
		ref.ImageMessage = proto.Clone(msg.GetImageMessage()).(*waE2E.ImageMessage)
		ref.ImageMessage.JPEGThumbnail, ref.ImageMessage.ContextInfo = nil, nil
	case msg.GetVideoMessage() != nil, msg.GetPtvMessage() != nil:
		video := msg.GetVideoMessage()
		if video == nil {
			video = msg.GetPtvMessage()
		}
		ref.VideoMessage = proto.Clone(video).(*waE2E.VideoMessage)
		ref.VideoMessage.JPEGThumbnail, ref.VideoMessage.ContextInfo = nil, nil
	case msg.GetAudioMessage() != nil:
		ref.AudioMessage = proto.Clone(msg.GetAudioMessage()).(*waE2E.AudioMessage)
		ref.AudioMessage.ContextInfo, ref.AudioMessage.Waveform = nil, nil
	case msg.GetDocumentMessage() != nil, msg.GetDocumentWithCaptionMessage().GetMessage().GetDocumentMessage() != nil:
		doc := msg.GetDocumentMessage()
		if doc == nil {
			doc = msg.GetDocumentWithCaptionMessage().GetMessage().GetDocumentMessage()
		}
		ref.DocumentMessage = proto.Clone(doc).(*waE2E.DocumentMessage)
		ref.DocumentMessage.JPEGThumbnail, ref.DocumentMessage.ContextInfo = nil, nil
	case msg.GetStickerMessage() != nil:
		ref.StickerMessage = proto.Clone(msg.GetStickerMessage()).(*waE2E.StickerMessage)
		ref.StickerMessage.PngThumbnail, ref.StickerMessage.ContextInfo = nil, nil
	default:
		return nil
	}

	data, err := proto.Marshal(ref)
	if err != nil {
		return nil
	}
	return data
}

// resolveChat returns the canonical chat JID. WhatsApp increasingly addresses
// 1:1 chats by LID (xxx@lid); we key them by phone number whenever it can be
// resolved so one conversation does not split in two.
func resolveChat(client *whatsmeow.Client, info *types.MessageInfo) string {
	chat := info.Chat.ToNonAD()
	if chat.Server != types.HiddenUserServer {
		return chat.String()
	}

	alt := info.SenderAlt
	if info.IsFromMe {
		alt = info.RecipientAlt
	}
	if alt.Server == types.DefaultUserServer && alt.User != "" {
		return alt.ToNonAD().String()
	}
	if pn, ok := pnForLID(client, chat); ok {
		return pn.String()
	}
	return chat.String()
}

func resolveSender(client *whatsmeow.Client, info *types.MessageInfo) string {
	sender := info.Sender.ToNonAD()
	if sender.Server != types.HiddenUserServer {
		return sender.String()
	}
	if alt := info.SenderAlt; alt.Server == types.DefaultUserServer && alt.User != "" {
		return alt.ToNonAD().String()
	}
	if pn, ok := pnForLID(client, sender); ok {
		return pn.String()
	}
	return sender.String()
}

func pnForLID(client *whatsmeow.Client, lid types.JID) (types.JID, bool) {
	if client == nil || client.Store == nil || client.Store.LIDs == nil {
		return types.JID{}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	pn, err := client.Store.LIDs.GetPNForLID(ctx, lid)
	if err != nil || pn.User == "" {
		return types.JID{}, false
	}
	return pn.ToNonAD(), true
}

// describe maps a WhatsApp message to a short type label plus the text worth
// keeping. Media bodies are never stored, only captions, file names and a
// reference that lets the file be fetched again (see mediaRef). An
// empty type means "do not store".
func describe(msg *waE2E.Message) (msgType, text string) {
	switch {
	case msg == nil:
		return "", ""
	case msg.GetConversation() != "":
		return "text", msg.GetConversation()
	case msg.GetExtendedTextMessage() != nil:
		return "text", msg.GetExtendedTextMessage().GetText()
	case msg.GetImageMessage() != nil:
		return "image", msg.GetImageMessage().GetCaption()
	case msg.GetVideoMessage() != nil:
		return "video", msg.GetVideoMessage().GetCaption()
	case msg.GetPtvMessage() != nil:
		return "video_note", ""
	case msg.GetAudioMessage() != nil:
		// The text of an audio message is its transcript, filled in later.
		if msg.GetAudioMessage().GetPTT() {
			return "voice_note", ""
		}
		return "audio", ""
	case msg.GetDocumentMessage() != nil:
		d := msg.GetDocumentMessage()
		return "document", joinNonEmpty(" — ", d.GetFileName(), d.GetCaption())
	case msg.GetDocumentWithCaptionMessage().GetMessage().GetDocumentMessage() != nil:
		d := msg.GetDocumentWithCaptionMessage().GetMessage().GetDocumentMessage()
		return "document", joinNonEmpty(" — ", d.GetFileName(), d.GetCaption())
	case msg.GetStickerMessage() != nil:
		return "sticker", ""
	case msg.GetLocationMessage() != nil:
		l := msg.GetLocationMessage()
		coords := fmt.Sprintf("%.6f,%.6f", l.GetDegreesLatitude(), l.GetDegreesLongitude())
		return "location", joinNonEmpty(" — ", l.GetName(), l.GetAddress(), coords)
	case msg.GetLiveLocationMessage() != nil:
		l := msg.GetLiveLocationMessage()
		coords := fmt.Sprintf("%.6f,%.6f", l.GetDegreesLatitude(), l.GetDegreesLongitude())
		return "live_location", joinNonEmpty(" — ", l.GetCaption(), coords)
	case msg.GetContactMessage() != nil:
		return "contact", msg.GetContactMessage().GetDisplayName()
	case msg.GetContactsArrayMessage() != nil:
		var names []string
		for _, c := range msg.GetContactsArrayMessage().GetContacts() {
			names = append(names, c.GetDisplayName())
		}
		return "contact", strings.Join(names, ", ")
	case msg.GetButtonsResponseMessage() != nil:
		return "button_reply", msg.GetButtonsResponseMessage().GetSelectedDisplayText()
	case msg.GetTemplateButtonReplyMessage() != nil:
		return "button_reply", msg.GetTemplateButtonReplyMessage().GetSelectedDisplayText()
	case msg.GetListResponseMessage() != nil:
		return "list_reply", msg.GetListResponseMessage().GetTitle()
	}

	if poll := pollCreation(msg); poll != nil {
		return "poll", joinNonEmpty(" — ", poll.GetName(), strings.Join(pollOptions(msg), " | "))
	}

	return "", ""
}

func pollOptions(msg *waE2E.Message) []string {
	var options []string
	for _, o := range pollCreation(msg).GetOptions() {
		options = append(options, o.GetOptionName())
	}
	return options
}

func pollCreation(msg *waE2E.Message) *waE2E.PollCreationMessage {
	for _, p := range []*waE2E.PollCreationMessage{
		msg.GetPollCreationMessage(),
		msg.GetPollCreationMessageV2(),
		msg.GetPollCreationMessageV3(),
		msg.GetPollCreationMessageV5(),
		msg.GetPollCreationMessageV6(),
	} {
		if p != nil {
			return p
		}
	}
	return nil
}

func quotedID(msg *waE2E.Message) string {
	switch {
	case msg.GetExtendedTextMessage() != nil:
		return msg.GetExtendedTextMessage().GetContextInfo().GetStanzaID()
	case msg.GetImageMessage() != nil:
		return msg.GetImageMessage().GetContextInfo().GetStanzaID()
	case msg.GetVideoMessage() != nil:
		return msg.GetVideoMessage().GetContextInfo().GetStanzaID()
	case msg.GetAudioMessage() != nil:
		return msg.GetAudioMessage().GetContextInfo().GetStanzaID()
	case msg.GetDocumentMessage() != nil:
		return msg.GetDocumentMessage().GetContextInfo().GetStanzaID()
	case msg.GetStickerMessage() != nil:
		return msg.GetStickerMessage().GetContextInfo().GetStanzaID()
	}
	return ""
}

func joinNonEmpty(sep string, parts ...string) string {
	var kept []string
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, sep)
}
