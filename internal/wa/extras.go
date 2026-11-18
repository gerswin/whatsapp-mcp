package wa

import (
	"context"
	"crypto/sha256"
	"errors"
	"sort"
	"strconv"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/gerswin/whatsapp-mcp/internal/store"
)

// statusChatName is how the chat holding status updates is listed.
const statusChatName = "Status updates"

func isStatus(chat types.JID) bool {
	return chat.Server == types.BroadcastServer && chat.User == types.StatusBroadcastJID.User
}

// ------------------------------------------------------------- reactions

// captureReaction stores a reaction event. It reports whether evt was a
// reaction, so the caller does not treat it as a message.
func (m *Manager) captureReaction(client *whatsmeow.Client, account string, evt *events.Message) bool {
	reaction := evt.Message.GetReactionMessage()
	if reaction == nil {
		if evt.Message.GetEncReactionMessage() == nil {
			return false
		}
		// Community announcement groups encrypt reactions with the secret of
		// the message they point at.
		if client == nil {
			return true
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		decrypted, err := client.DecryptReaction(ctx, evt)
		if err != nil {
			m.log.Debug("could not decrypt reaction", "account", account, "err", err)
			return true
		}
		reaction = decrypted
	}

	chat := resolveChat(client, &evt.Info)
	if err := m.store.SetReaction(account, chat, reaction.GetKey().GetID(), resolveSender(client, &evt.Info),
		evt.Info.IsFromMe, reaction.GetText(), timeOrNow(evt.Info.Timestamp)); err != nil {
		m.log.Error("failed to save reaction", "account", account, "err", err)
	}
	return true
}

// ------------------------------------------------------------- poll votes

// capturePollVote stores a poll vote event. It reports whether evt was a
// vote, so the caller does not treat it as a message.
func (m *Manager) capturePollVote(client *whatsmeow.Client, account string, evt *events.Message) bool {
	update := evt.Message.GetPollUpdateMessage()
	if update == nil {
		return false
	}
	if client == nil {
		return true
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	vote, err := client.DecryptPollVote(ctx, evt)
	if err != nil {
		// Votes are encrypted with a secret that came with the poll itself, so
		// polls created before this device was linked may not be readable.
		m.log.Debug("could not decrypt poll vote", "account", account, "err", err)
		return true
	}

	m.savePollVote(account, resolveChat(client, &evt.Info), update.GetPollCreationMessageKey().GetID(),
		resolveSender(client, &evt.Info), evt.Info.IsFromMe, vote.GetSelectedOptions(), timeOrNow(evt.Info.Timestamp))
	return true
}

// savePollVote stores a vote. WhatsApp identifies the chosen options by the
// SHA-256 of their names, so the poll has to be in the store to name them.
func (m *Manager) savePollVote(account, chat, pollID, voter string, fromMe bool, selected [][]byte, ts time.Time) {
	options := m.store.PollOptions(account, chat, pollID)
	if len(options) == 0 {
		return
	}

	chosen := map[[sha256.Size]byte]bool{}
	for _, hash := range selected {
		if len(hash) == sha256.Size {
			chosen[[sha256.Size]byte(hash)] = true
		}
	}
	names := []string{}
	for _, option := range options {
		if chosen[sha256.Sum256([]byte(option))] {
			names = append(names, option)
		}
	}

	if err := m.store.SetPollVote(account, chat, pollID, voter, fromMe, names, ts); err != nil {
		m.log.Error("failed to save poll vote", "account", account, "poll", pollID, "err", err)
	}
}

// ------------------------------------------------------------- history

// captureHistoryExtras stores the reactions and poll votes that history sync
// attaches to a message. The message itself must already be saved.
func (m *Manager) captureHistoryExtras(client *whatsmeow.Client, account string, msg store.Message, web *waWeb.WebMessageInfo) {
	for _, r := range web.GetReactions() {
		actor, fromMe := historyActor(client, msg.ChatJID, r.GetKey())
		if err := m.store.SetReaction(account, msg.ChatJID, msg.ID, actor, fromMe, r.GetText(),
			millisOr(r.GetSenderTimestampMS(), msg.Timestamp)); err != nil {
			m.log.Error("failed to save reaction", "account", account, "err", err)
		}
	}
	for _, u := range web.GetPollUpdates() {
		if u.GetVote() == nil {
			continue
		}
		actor, fromMe := historyActor(client, msg.ChatJID, u.GetPollUpdateMessageKey())
		m.savePollVote(account, msg.ChatJID, msg.ID, actor, fromMe, u.GetVote().GetSelectedOptions(),
			millisOr(u.GetSenderTimestampMS(), msg.Timestamp))
	}
}

// historyActor works out who sent a reaction or vote from its message key.
func historyActor(client *whatsmeow.Client, chat string, key *waCommon.MessageKey) (jid string, fromMe bool) {
	if key.GetFromMe() {
		if client != nil && client.Store != nil && client.Store.ID != nil {
			return client.Store.ID.ToNonAD().String(), true
		}
		return "me", true
	}

	raw := key.GetParticipant()
	if raw == "" {
		raw = key.GetRemoteJID()
	}
	parsed, err := types.ParseJID(raw)
	if err != nil || parsed.User == "" {
		return chat, false // 1:1 chat: the other side
	}
	parsed = parsed.ToNonAD()
	if parsed.Server == types.HiddenUserServer {
		if pn, ok := pnForLID(client, parsed); ok {
			parsed = pn
		}
	}
	return parsed.String(), false
}

func timeOrNow(ts time.Time) time.Time {
	if ts.IsZero() {
		return time.Now()
	}
	return ts
}

func millisOr(ms int64, fallback time.Time) time.Time {
	if ms <= 0 {
		return fallback
	}
	return time.UnixMilli(ms)
}

// ------------------------------------------------------------- channels

// Channel is a WhatsApp channel (newsletter) the account follows.
type Channel struct {
	JID         string
	Name        string
	Description string
	Subscribers int
	Role        string
	Verified    bool
}

func (m *Manager) Channels(ctx context.Context, account string) ([]Channel, error) {
	client, err := m.ready(account)
	if err != nil {
		return nil, err
	}

	subscribed, err := client.GetSubscribedNewsletters(ctx)
	if err != nil {
		return nil, err
	}

	channels := make([]Channel, 0, len(subscribed))
	for _, n := range subscribed {
		if n == nil {
			continue
		}
		c := Channel{
			JID:         n.ID.String(),
			Name:        n.ThreadMeta.Name.Text,
			Description: n.ThreadMeta.Description.Text,
			Subscribers: n.ThreadMeta.SubscriberCount,
			Verified:    n.ThreadMeta.VerificationState == types.NewsletterVerificationStateVerified,
		}
		if n.ViewerMeta != nil {
			c.Role = string(n.ViewerMeta.Role)
		}
		channels = append(channels, c)
		_ = m.store.SetChatName(account, c.JID, c.Name)
	}
	return channels, nil
}

// ChannelMessage is one post of a channel, as WhatsApp reports it right now.
type ChannelMessage struct {
	// ID is the channel's own sequence number; pass it as "before" to page back.
	ID        int
	Timestamp time.Time
	Type      string
	Text      string
	HasMedia  bool
	Views     int
	// Reactions counts reactions per emoji; channels do not say who reacted.
	Reactions map[string]int
}

// ChannelMessages fetches posts of a channel from WhatsApp, oldest first.
// Channels are not captured as they arrive; the posts read here are saved so
// their photos and audio can be opened with Media and they can be searched.
func (m *Manager) ChannelMessages(ctx context.Context, account, channel string, limit, before int) ([]ChannelMessage, error) {
	client, err := m.ready(account)
	if err != nil {
		return nil, err
	}
	jid, err := types.ParseJID(channel)
	if err != nil || jid.User == "" || jid.Server != types.NewsletterServer {
		return nil, errors.New("channel must be a channel JID ending in @newsletter (see list_channels)")
	}

	fetched, err := client.GetNewsletterMessages(ctx, jid, &whatsmeow.GetNewsletterMessagesParams{Count: limit, Before: before})
	if err != nil {
		return nil, err
	}

	chat := jid.String()
	out := make([]ChannelMessage, 0, len(fetched))
	stored := make([]store.Message, 0, len(fetched))
	for _, f := range fetched {
		if f == nil || f.Message == nil {
			continue
		}
		msgType, text := describe(f.Message)
		if msgType == "" {
			continue
		}
		ref := mediaRef(f.Message)

		out = append(out, ChannelMessage{
			ID:        f.MessageServerID,
			Timestamp: f.Timestamp,
			Type:      msgType,
			Text:      text,
			HasMedia:  ref != nil,
			Views:     f.ViewsCount,
			Reactions: f.ReactionCounts,
		})
		stored = append(stored, store.Message{
			ID:          strconv.Itoa(f.MessageServerID),
			ChatJID:     chat,
			Type:        msgType,
			Text:        text,
			Timestamp:   timeOrNow(f.Timestamp),
			Media:       ref,
			PollOptions: pollOptions(f.Message),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })

	if err := m.store.Save(account, stored...); err != nil {
		m.log.Error("failed to save channel messages", "account", account, "channel", chat, "err", err)
	}
	return out, nil
}
