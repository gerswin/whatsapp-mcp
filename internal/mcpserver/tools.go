package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gerswin/whatsapp-mcp/internal/media"
	"github.com/gerswin/whatsapp-mcp/internal/store"
	"github.com/gerswin/whatsapp-mcp/internal/wa"
)

// tools holds the per-request state: the WhatsApp backend, the message store
// and the account the caller authenticated as.
type tools struct {
	Backend
	store   *store.Store
	account string
}

func (t *tools) register(s *mcp.Server, voiceNotes bool) {
	addTool(s, "get_status", "Connection status",
		"Report whether this WhatsApp account is connected and logged in, and which number it is.",
		readOnly, t.getStatus)

	addTool(s, "list_chats", "List chats",
		"List conversations ordered by most recent activity, with a preview of the last message. Use it to find a chat's JID.",
		readOnly, t.listChats)
	addTool(s, "get_messages", "Read chat messages",
		"Read the messages of one chat, oldest first, with their reactions and, for polls, the current results. Returns the most recent messages by default; pass 'before' to page further back.",
		readOnly, t.getMessages)
	addTool(s, "search_messages", "Search messages",
		"Find messages containing a text fragment (case-insensitive), across all chats or within one chat. Transcripts of voice notes are searched too. Newest first.",
		readOnly, t.searchMessages)
	t.addGetMedia(s)

	addTool(s, "list_contacts", "List contacts",
		"List the contacts saved on this WhatsApp account, optionally filtered by name or number.",
		readOnly, t.listContacts)
	addTool(s, "check_numbers", "Check numbers on WhatsApp",
		"Check which phone numbers are registered on WhatsApp and get their JIDs.",
		readOnly, t.checkNumbers)
	addTool(s, "list_groups", "List groups",
		"List the groups this account belongs to.",
		readOnly, t.listGroups)
	addTool(s, "get_group_info", "Group details",
		"Get a group's name, description and participants.",
		readOnly, t.getGroupInfo)
	addTool(s, "list_channels", "List channels",
		"List the WhatsApp channels this account follows.",
		readOnly, t.listChannels)
	addTool(s, "get_channel_messages", "Read channel posts",
		"Read the posts of a channel, oldest first, fetched from WhatsApp at this moment. Returns the most recent posts by default; pass 'before' to page further back. Posts with has_media can be opened with get_media, using the channel JID as chat.",
		readOnly, t.getChannelMessages)

	addTool(s, "send_text", "Send text message",
		"Send a text message to a person or group. Optionally reply to (quote) a previous message.",
		sends, t.sendText)
	addTool(s, "send_media", "Send media from URL",
		"Send an image, video, audio or document that is available at a public URL.",
		sends, t.sendMedia)
	if voiceNotes {
		addTool(s, "send_voice_note", "Send voice note",
			"Speak a text with this server's synthetic voice and send it as a voice note to a person or group. Write the text the way it should be said aloud, in the recipient's language, without emojis, markdown or URLs.",
			sends, t.sendVoiceNote)
	}
	addTool(s, "send_location", "Send location",
		"Send a location pin to a person or group.",
		sends, t.sendLocation)
	addTool(s, "send_poll", "Send poll",
		"Send a poll to a person or group.",
		sends, t.sendPoll)
	addTool(s, "react_to_message", "React to message",
		"Add an emoji reaction to a message, or remove it by passing an empty emoji.",
		sends, t.react)
	addTool(s, "mark_read", "Mark messages as read",
		"Mark messages in a chat as read (sends read receipts to the other side).",
		sends, t.markRead)
}

type toolKind int

const (
	readOnly toolKind = iota
	sends
)

// addTool registers a typed tool, hiding the request/result plumbing the
// handlers here never need.
func addTool[In, Out any](s *mcp.Server, name, title, description string, kind toolKind, fn func(context.Context, In) (Out, error)) {
	notDestructive := false
	openWorld := true
	annotations := &mcp.ToolAnnotations{Title: title, OpenWorldHint: &openWorld}
	if kind == readOnly {
		annotations.ReadOnlyHint = true
	} else {
		// These add messages/receipts; they never modify or delete data.
		annotations.DestructiveHint = &notDestructive
	}

	mcp.AddTool(s, &mcp.Tool{
		Name:        name,
		Title:       title,
		Description: description,
		Annotations: annotations,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		out, err := fn(ctx, in)
		return nil, out, err
	})
}

// ---------------------------------------------------------------- status

type statusOutput struct {
	Account   string `json:"account" jsonschema:"account name on this server"`
	Connected bool   `json:"connected" jsonschema:"connected to WhatsApp servers"`
	LoggedIn  bool   `json:"logged_in" jsonschema:"linked to a WhatsApp account"`
	JID       string `json:"jid,omitempty" jsonschema:"JID of the linked account"`
	Name      string `json:"name,omitempty" jsonschema:"profile name of the linked account"`
}

func (t *tools) getStatus(_ context.Context, _ struct{}) (statusOutput, error) {
	st := t.Status(t.account)
	return statusOutput{Account: st.Account, Connected: st.Connected, LoggedIn: st.LoggedIn, JID: st.JID, Name: st.Name}, nil
}

// ---------------------------------------------------------------- history

type chatOutput struct {
	JID           string `json:"jid"`
	Name          string `json:"name,omitempty"`
	IsGroup       bool   `json:"is_group"`
	MessageCount  int    `json:"message_count" jsonschema:"number of stored messages"`
	LastMessageAt string `json:"last_message_at" jsonschema:"RFC 3339 timestamp"`
	LastMessage   string `json:"last_message" jsonschema:"preview of the most recent message"`
}

type listChatsInput struct {
	Query string `json:"query,omitempty" jsonschema:"filter by chat name or number"`
	Limit int    `json:"limit,omitempty" jsonschema:"maximum chats to return (default 30, max 200)"`
}

type listChatsOutput struct {
	Chats []chatOutput `json:"chats"`
}

func (t *tools) listChats(ctx context.Context, in listChatsInput) (listChatsOutput, error) {
	chats, err := t.store.ListChats(t.account, strings.TrimSpace(in.Query), limitOr(in.Limit, 30, 200))
	if err != nil {
		return listChatsOutput{}, err
	}

	out := listChatsOutput{Chats: make([]chatOutput, 0, len(chats))}
	for _, c := range chats {
		preview := c.LastMessageText
		if preview == "" {
			preview = "[" + c.LastMessageType + "]"
		} else if c.LastMessageType != "text" {
			preview = "[" + c.LastMessageType + "] " + preview
		}
		if c.LastFromMe {
			preview = "You: " + preview
		}

		out.Chats = append(out.Chats, chatOutput{
			JID:           c.JID,
			Name:          t.chatName(ctx, c.JID, c.Name),
			IsGroup:       isGroupJID(c.JID),
			MessageCount:  c.MessageCount,
			LastMessageAt: c.LastMessageAt.Format(time.RFC3339),
			LastMessage:   truncate(preview, 200),
		})
	}
	return out, nil
}

type messageOutput struct {
	ID         string `json:"id"`
	Chat       string `json:"chat,omitempty" jsonschema:"chat JID"`
	ChatName   string `json:"chat_name,omitempty"`
	Timestamp  string `json:"timestamp" jsonschema:"RFC 3339 timestamp"`
	FromMe     bool   `json:"from_me" jsonschema:"true if sent by this account"`
	Sender     string `json:"sender,omitempty" jsonschema:"sender JID"`
	SenderName string `json:"sender_name,omitempty"`
	Type       string `json:"type" jsonschema:"text, image, video, audio, voice_note, document, sticker, location, contact, poll, ..."`
	Text       string `json:"text,omitempty" jsonschema:"message text; the caption or file name for media; the automatic transcript for voice notes and audio"`
	HasMedia   bool   `json:"has_media,omitempty" jsonschema:"the file of this message can be opened with get_media"`
	QuotedID   string `json:"quoted_id,omitempty" jsonschema:"id of the message this one replies to"`
	Deleted    bool   `json:"deleted,omitempty" jsonschema:"the sender deleted this message for everyone"`

	Reactions   []reactionOutput   `json:"reactions,omitempty" jsonschema:"emoji reactions currently on this message"`
	PollResults []pollOptionOutput `json:"poll_results,omitempty" jsonschema:"for polls: every option with the votes known so far"`
}

type personOutput struct {
	FromMe bool   `json:"from_me,omitempty" jsonschema:"true if it is this account"`
	JID    string `json:"jid,omitempty"`
	Name   string `json:"name,omitempty"`
}

type reactionOutput struct {
	Emoji string `json:"emoji"`
	personOutput
}

type pollOptionOutput struct {
	Option string         `json:"option"`
	Votes  int            `json:"votes"`
	Voters []personOutput `json:"voters,omitempty"`
}

type getMessagesInput struct {
	Chat   string `json:"chat" jsonschema:"chat JID (from list_chats) or phone number"`
	Limit  int    `json:"limit,omitempty" jsonschema:"maximum messages to return (default 50, max 500)"`
	Before string `json:"before,omitempty" jsonschema:"only messages older than this RFC 3339 timestamp; use the oldest timestamp of the previous page to go further back"`
}

type getMessagesOutput struct {
	Chat     string          `json:"chat"`
	ChatName string          `json:"chat_name,omitempty"`
	Messages []messageOutput `json:"messages"`
}

func (t *tools) getMessages(ctx context.Context, in getMessagesInput) (getMessagesOutput, error) {
	chat, err := chatJID(in.Chat)
	if err != nil {
		return getMessagesOutput{}, err
	}

	var before time.Time
	if in.Before != "" {
		if before, err = time.Parse(time.RFC3339, in.Before); err != nil {
			return getMessagesOutput{}, errors.New("'before' must be an RFC 3339 timestamp, e.g. 2026-01-31T15:04:05Z")
		}
	}

	msgs, err := t.store.Messages(t.account, chat, before, limitOr(in.Limit, 50, 500))
	if err != nil {
		return getMessagesOutput{}, err
	}
	if err := t.store.Annotate(t.account, msgs); err != nil {
		return getMessagesOutput{}, err
	}
	people := t.people(ctx)

	out := getMessagesOutput{
		Chat:     chat,
		ChatName: t.chatName(ctx, chat, t.store.ChatName(t.account, chat)),
		Messages: make([]messageOutput, 0, len(msgs)),
	}
	for _, m := range msgs {
		out.Messages = append(out.Messages, toMessageOutput(m, false, people))
	}
	return out, nil
}

type searchMessagesInput struct {
	Query string `json:"query" jsonschema:"text to look for"`
	Chat  string `json:"chat,omitempty" jsonschema:"restrict the search to this chat JID or phone number"`
	Limit int    `json:"limit,omitempty" jsonschema:"maximum messages to return (default 30, max 200)"`
}

type searchMessagesOutput struct {
	Messages []messageOutput `json:"messages"`
}

func (t *tools) searchMessages(ctx context.Context, in searchMessagesInput) (searchMessagesOutput, error) {
	query := strings.TrimSpace(in.Query)
	if query == "" {
		return searchMessagesOutput{}, errors.New("query is required")
	}

	chat := ""
	if strings.TrimSpace(in.Chat) != "" {
		var err error
		if chat, err = chatJID(in.Chat); err != nil {
			return searchMessagesOutput{}, err
		}
	}

	msgs, err := t.store.Search(t.account, query, chat, limitOr(in.Limit, 30, 200))
	if err != nil {
		return searchMessagesOutput{}, err
	}
	if err := t.store.Annotate(t.account, msgs); err != nil {
		return searchMessagesOutput{}, err
	}
	people := t.people(ctx)

	names := map[string]string{}
	out := searchMessagesOutput{Messages: make([]messageOutput, 0, len(msgs))}
	for _, m := range msgs {
		name, ok := names[m.ChatJID]
		if !ok {
			name = t.chatName(ctx, m.ChatJID, t.store.ChatName(t.account, m.ChatJID))
			names[m.ChatJID] = name
		}
		mo := toMessageOutput(m, true, people)
		mo.ChatName = name
		out.Messages = append(out.Messages, mo)
	}
	return out, nil
}

// people returns a lookup from JID to address-book name that remembers its
// answers, so a page of messages asks about each person once.
func (t *tools) people(ctx context.Context) func(jid string) string {
	names := map[string]string{}
	return func(jid string) string {
		name, ok := names[jid]
		if !ok {
			name = t.ContactName(ctx, t.account, jid)
			names[jid] = name
		}
		return name
	}
}

func person(jid string, fromMe bool, name func(string) string) personOutput {
	if fromMe {
		return personOutput{FromMe: true}
	}
	return personOutput{JID: jid, Name: name(jid)}
}

func toMessageOutput(m store.Message, withChat bool, name func(string) string) messageOutput {
	out := messageOutput{
		ID:        m.ID,
		Timestamp: m.Timestamp.Format(time.RFC3339),
		FromMe:    m.FromMe,
		Type:      m.Type,
		Text:      m.Text,
		QuotedID:  m.QuotedID,
		Deleted:   m.Deleted,
		HasMedia:  m.HasMedia && !m.Deleted,
	}
	if withChat {
		out.Chat = m.ChatJID
	}
	if !m.FromMe && m.SenderJID != "" {
		out.Sender = m.SenderJID
		out.SenderName = firstNonEmpty(name(m.SenderJID), m.SenderName)
	}

	for _, r := range m.Reactions {
		out.Reactions = append(out.Reactions, reactionOutput{Emoji: r.Emoji, personOutput: person(r.SenderJID, r.FromMe, name)})
	}
	for _, option := range m.PollOptions {
		result := pollOptionOutput{Option: option}
		for _, v := range m.Votes {
			for _, chosen := range v.Options {
				if chosen == option {
					result.Votes++
					result.Voters = append(result.Voters, person(v.VoterJID, v.FromMe, name))
				}
			}
		}
		out.PollResults = append(out.PollResults, result)
	}
	return out
}

// chatName returns stored if set; otherwise it falls back to the WhatsApp
// address book for 1:1 chats.
func (t *tools) chatName(ctx context.Context, jid, stored string) string {
	if stored != "" || isGroupJID(jid) {
		return stored
	}
	return t.ContactName(ctx, t.account, jid)
}

// ---------------------------------------------------------------- media

type getMediaInput struct {
	Chat      string `json:"chat" jsonschema:"chat JID or phone number the message belongs to"`
	MessageID string `json:"message_id" jsonschema:"id of a message that has has_media set"`
}

// addGetMedia registers get_media by hand: unlike the other tools it returns
// content blocks (a picture) instead of a JSON object.
func (t *tools) addGetMedia(s *mcp.Server) {
	openWorld := true
	mcp.AddTool(s, &mcp.Tool{
		Name:  "get_media",
		Title: "Open a photo or audio",
		Description: "Open the file attached to a message. Images and stickers are returned as a picture you can look at; " +
			"voice notes, audio and videos are returned as a transcript of what is said; plain-text documents as their text. " +
			"Other documents (PDF, Office) cannot be read yet.",
		Annotations: &mcp.ToolAnnotations{Title: "Open a photo or audio", ReadOnlyHint: true, OpenWorldHint: &openWorld},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in getMediaInput) (*mcp.CallToolResult, any, error) {
		if strings.TrimSpace(in.MessageID) == "" {
			return nil, nil, errors.New("message_id is required")
		}
		m, err := t.Media(ctx, t.account, in.Chat, in.MessageID)
		if err != nil {
			return nil, nil, err
		}

		res := &mcp.CallToolResult{}
		switch {
		case len(m.Image) > 0:
			res.Content = []mcp.Content{
				&mcp.ImageContent{Data: m.Image, MIMEType: "image/jpeg"},
				&mcp.TextContent{Text: "The " + m.Kind + " from message " + in.MessageID + " is attached."},
			}
		case m.Text != "":
			label := "Transcript"
			if m.Kind == "document" {
				label = "Content of " + firstNonEmpty(m.Filename, "the document")
			}
			res.Content = []mcp.Content{&mcp.TextContent{Text: label + " (" + m.Kind + "):\n\n" + m.Text}}
		default:
			res.Content = []mcp.Content{&mcp.TextContent{Text: "No speech was recognized in that " + m.Kind + "."}}
		}
		return res, nil, nil
	})
}

// ---------------------------------------------------------------- contacts

type contactOutput struct {
	JID          string `json:"jid"`
	Name         string `json:"name,omitempty" jsonschema:"name saved in the address book"`
	PushName     string `json:"push_name,omitempty" jsonschema:"name the contact chose for their own profile"`
	BusinessName string `json:"business_name,omitempty"`
}

type listContactsInput struct {
	Query string `json:"query,omitempty" jsonschema:"filter by name or number"`
	Limit int    `json:"limit,omitempty" jsonschema:"maximum contacts to return (default 100, max 1000)"`
}

type listContactsOutput struct {
	Contacts []contactOutput `json:"contacts"`
	Total    int             `json:"total" jsonschema:"number of contacts matching the filter, before the limit"`
}

func (t *tools) listContacts(ctx context.Context, in listContactsInput) (listContactsOutput, error) {
	contacts, err := t.Contacts(ctx, t.account)
	if err != nil {
		return listContactsOutput{}, err
	}

	query := strings.ToLower(strings.TrimSpace(in.Query))
	matched := make([]contactOutput, 0, len(contacts))
	for _, c := range contacts {
		if query != "" && !containsAny(query, c.Name, c.PushName, c.BusinessName, c.JID) {
			continue
		}
		matched = append(matched, contactOutput{JID: c.JID, Name: c.Name, PushName: c.PushName, BusinessName: c.BusinessName})
	}

	sort.Slice(matched, func(i, j int) bool {
		a := strings.ToLower(firstNonEmpty(matched[i].Name, matched[i].PushName, matched[i].JID))
		b := strings.ToLower(firstNonEmpty(matched[j].Name, matched[j].PushName, matched[j].JID))
		return a < b
	})

	out := listContactsOutput{Total: len(matched)}
	if limit := limitOr(in.Limit, 100, 1000); len(matched) > limit {
		matched = matched[:limit]
	}
	out.Contacts = matched
	return out, nil
}

type checkNumbersInput struct {
	Numbers []string `json:"numbers" jsonschema:"phone numbers in international format, with country code"`
}

type checkedNumber struct {
	Query        string `json:"query"`
	OnWhatsApp   bool   `json:"on_whatsapp"`
	JID          string `json:"jid,omitempty"`
	VerifiedName string `json:"verified_name,omitempty" jsonschema:"verified business name, if any"`
}

type checkNumbersOutput struct {
	Numbers []checkedNumber `json:"numbers"`
}

func (t *tools) checkNumbers(ctx context.Context, in checkNumbersInput) (checkNumbersOutput, error) {
	if len(in.Numbers) == 0 {
		return checkNumbersOutput{}, errors.New("numbers is required")
	}
	if len(in.Numbers) > 50 {
		return checkNumbersOutput{}, errors.New("check at most 50 numbers at a time")
	}

	result, err := t.CheckNumbers(ctx, t.account, in.Numbers)
	if err != nil {
		return checkNumbersOutput{}, err
	}

	out := checkNumbersOutput{Numbers: make([]checkedNumber, 0, len(result))}
	for _, n := range result {
		out.Numbers = append(out.Numbers, checkedNumber{Query: n.Query, OnWhatsApp: n.OnWhatsApp, JID: n.JID, VerifiedName: n.VerifiedName})
	}
	return out, nil
}

// ---------------------------------------------------------------- groups

type groupSummary struct {
	JID          string `json:"jid"`
	Name         string `json:"name"`
	Participants int    `json:"participants"`
}

type listGroupsOutput struct {
	Groups []groupSummary `json:"groups"`
}

func (t *tools) listGroups(ctx context.Context, _ struct{}) (listGroupsOutput, error) {
	groups, err := t.Groups(ctx, t.account)
	if err != nil {
		return listGroupsOutput{}, err
	}

	out := listGroupsOutput{Groups: make([]groupSummary, 0, len(groups))}
	for _, g := range groups {
		out.Groups = append(out.Groups, groupSummary{JID: g.JID, Name: g.Name, Participants: g.Participants})
	}
	sort.Slice(out.Groups, func(i, j int) bool {
		return strings.ToLower(out.Groups[i].Name) < strings.ToLower(out.Groups[j].Name)
	})
	return out, nil
}

type getGroupInfoInput struct {
	Group string `json:"group" jsonschema:"group JID (ends in @g.us)"`
}

type groupParticipant struct {
	JID     string `json:"jid"`
	Phone   string `json:"phone,omitempty"`
	IsAdmin bool   `json:"is_admin"`
}

type getGroupInfoOutput struct {
	JID          string             `json:"jid"`
	Name         string             `json:"name"`
	Description  string             `json:"description,omitempty"`
	Owner        string             `json:"owner,omitempty"`
	CreatedAt    string             `json:"created_at,omitempty" jsonschema:"RFC 3339 timestamp"`
	AdminsOnly   bool               `json:"admins_only" jsonschema:"only admins can send messages"`
	Participants []groupParticipant `json:"participants"`
}

func (t *tools) getGroupInfo(ctx context.Context, in getGroupInfoInput) (getGroupInfoOutput, error) {
	g, err := t.GroupInfo(ctx, t.account, strings.TrimSpace(in.Group))
	if err != nil {
		return getGroupInfoOutput{}, err
	}

	out := getGroupInfoOutput{
		JID:          g.JID,
		Name:         g.Name,
		Description:  g.Description,
		Owner:        g.Owner,
		AdminsOnly:   g.AdminsOnly,
		Participants: make([]groupParticipant, 0, len(g.Participants)),
	}
	if !g.CreatedAt.IsZero() {
		out.CreatedAt = g.CreatedAt.Format(time.RFC3339)
	}
	for _, p := range g.Participants {
		out.Participants = append(out.Participants, groupParticipant{JID: p.JID, Phone: p.Phone, IsAdmin: p.IsAdmin})
	}
	return out, nil
}

// ---------------------------------------------------------------- channels

type channelOutput struct {
	JID         string `json:"jid"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Subscribers int    `json:"subscribers"`
	Verified    bool   `json:"verified,omitempty"`
	Role        string `json:"role,omitempty" jsonschema:"this account's role: subscriber, admin or owner"`
}

type listChannelsOutput struct {
	Channels []channelOutput `json:"channels"`
}

func (t *tools) listChannels(ctx context.Context, _ struct{}) (listChannelsOutput, error) {
	channels, err := t.Channels(ctx, t.account)
	if err != nil {
		return listChannelsOutput{}, err
	}

	out := listChannelsOutput{Channels: make([]channelOutput, 0, len(channels))}
	for _, c := range channels {
		out.Channels = append(out.Channels, channelOutput{
			JID: c.JID, Name: c.Name, Description: truncate(c.Description, 300),
			Subscribers: c.Subscribers, Verified: c.Verified, Role: c.Role,
		})
	}
	sort.Slice(out.Channels, func(i, j int) bool {
		return strings.ToLower(out.Channels[i].Name) < strings.ToLower(out.Channels[j].Name)
	})
	return out, nil
}

type getChannelMessagesInput struct {
	Channel string `json:"channel" jsonschema:"channel JID (ends in @newsletter), from list_channels"`
	Limit   int    `json:"limit,omitempty" jsonschema:"maximum posts to return (default 20, max 100)"`
	Before  int    `json:"before,omitempty" jsonschema:"only posts older than this post id; use the smallest id of the previous page to go further back"`
}

type channelMessageOutput struct {
	ID        string         `json:"id" jsonschema:"post id; also the message_id for get_media"`
	Timestamp string         `json:"timestamp" jsonschema:"RFC 3339 timestamp"`
	Type      string         `json:"type"`
	Text      string         `json:"text,omitempty"`
	HasMedia  bool           `json:"has_media,omitempty" jsonschema:"the file of this post can be opened with get_media"`
	Views     int            `json:"views,omitempty"`
	Reactions map[string]int `json:"reactions,omitempty" jsonschema:"number of reactions per emoji"`
}

type getChannelMessagesOutput struct {
	Channel  string                 `json:"channel"`
	Messages []channelMessageOutput `json:"messages"`
}

func (t *tools) getChannelMessages(ctx context.Context, in getChannelMessagesInput) (getChannelMessagesOutput, error) {
	channel := strings.TrimSpace(in.Channel)
	if in.Before < 0 {
		return getChannelMessagesOutput{}, errors.New("'before' must be a post id")
	}
	posts, err := t.ChannelMessages(ctx, t.account, channel, limitOr(in.Limit, 20, 100), in.Before)
	if err != nil {
		return getChannelMessagesOutput{}, err
	}

	out := getChannelMessagesOutput{Channel: channel, Messages: make([]channelMessageOutput, 0, len(posts))}
	for _, p := range posts {
		out.Messages = append(out.Messages, channelMessageOutput{
			ID:        strconv.Itoa(p.ID),
			Timestamp: p.Timestamp.Format(time.RFC3339),
			Type:      p.Type,
			Text:      p.Text,
			HasMedia:  p.HasMedia,
			Views:     p.Views,
			Reactions: p.Reactions,
		})
	}
	return out, nil
}

// ---------------------------------------------------------------- sending

type sentOutput struct {
	MessageID string `json:"message_id"`
	Chat      string `json:"chat" jsonschema:"JID the message was delivered to"`
	Timestamp string `json:"timestamp" jsonschema:"RFC 3339 timestamp"`
}

func sent(s wa.Sent, err error) (sentOutput, error) {
	if err != nil {
		return sentOutput{}, err
	}
	return sentOutput{MessageID: s.MessageID, Chat: s.Chat, Timestamp: s.Timestamp.Format(time.RFC3339)}, nil
}

type sendTextInput struct {
	To      string `json:"to" jsonschema:"recipient phone number or JID"`
	Text    string `json:"text" jsonschema:"message body; WhatsApp formatting (*bold*, _italic_) is supported"`
	ReplyTo string `json:"reply_to,omitempty" jsonschema:"id of a message in that chat to quote"`
}

func (t *tools) sendText(ctx context.Context, in sendTextInput) (sentOutput, error) {
	if strings.TrimSpace(in.Text) == "" {
		return sentOutput{}, errors.New("text is required")
	}
	return sent(t.SendText(ctx, t.account, in.To, in.Text, strings.TrimSpace(in.ReplyTo)))
}

type sendMediaInput struct {
	To       string `json:"to" jsonschema:"recipient phone number or JID"`
	URL      string `json:"url" jsonschema:"public http(s) URL of the file"`
	Type     string `json:"type" jsonschema:"one of: image, video, audio, document"`
	Caption  string `json:"caption,omitempty" jsonschema:"text shown with the media (not used for audio)"`
	Filename string `json:"filename,omitempty" jsonschema:"file name shown to the recipient (documents)"`
}

func (t *tools) sendMedia(ctx context.Context, in sendMediaInput) (sentOutput, error) {
	mediaType := strings.ToLower(strings.TrimSpace(in.Type))
	switch mediaType {
	case "image", "video", "audio", "document":
	default:
		return sentOutput{}, errors.New("type must be one of: image, video, audio, document")
	}
	if !strings.HasPrefix(in.URL, "http://") && !strings.HasPrefix(in.URL, "https://") {
		return sentOutput{}, errors.New("url must be an http(s) URL")
	}

	return sent(t.SendMedia(ctx, t.account, wa.MediaRequest{
		To:       in.To,
		URL:      in.URL,
		Type:     mediaType,
		Caption:  in.Caption,
		Filename: in.Filename,
	}))
}

type sendVoiceNoteInput struct {
	To      string `json:"to" jsonschema:"recipient phone number or JID"`
	Text    string `json:"text" jsonschema:"what the voice note says (up to 5000 characters)"`
	ReplyTo string `json:"reply_to,omitempty" jsonschema:"id of a message in that chat to quote"`
}

func (t *tools) sendVoiceNote(ctx context.Context, in sendVoiceNoteInput) (sentOutput, error) {
	text := strings.TrimSpace(in.Text)
	if text == "" {
		return sentOutput{}, errors.New("text is required")
	}
	if n := len([]rune(text)); n > media.MaxSpeechChars {
		return sentOutput{}, fmt.Errorf("text is too long for a voice note (%d characters, the limit is %d)", n, media.MaxSpeechChars)
	}
	return sent(t.SendVoiceNote(ctx, t.account, in.To, text, strings.TrimSpace(in.ReplyTo)))
}

type sendLocationInput struct {
	To        string  `json:"to" jsonschema:"recipient phone number or JID"`
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	Name      string  `json:"name,omitempty" jsonschema:"place name"`
	Address   string  `json:"address,omitempty"`
}

func (t *tools) sendLocation(ctx context.Context, in sendLocationInput) (sentOutput, error) {
	if in.Latitude < -90 || in.Latitude > 90 || in.Longitude < -180 || in.Longitude > 180 {
		return sentOutput{}, errors.New("latitude must be within ±90 and longitude within ±180")
	}
	return sent(t.SendLocation(ctx, t.account, in.To, in.Latitude, in.Longitude, in.Name, in.Address))
}

type sendPollInput struct {
	To         string   `json:"to" jsonschema:"recipient phone number or JID"`
	Question   string   `json:"question"`
	Options    []string `json:"options" jsonschema:"2 to 12 answer options"`
	MaxAnswers int      `json:"max_answers,omitempty" jsonschema:"how many options each person may pick (default 1)"`
}

func (t *tools) sendPoll(ctx context.Context, in sendPollInput) (sentOutput, error) {
	if strings.TrimSpace(in.Question) == "" {
		return sentOutput{}, errors.New("question is required")
	}
	if len(in.Options) < 2 || len(in.Options) > 12 {
		return sentOutput{}, errors.New("a poll needs between 2 and 12 options")
	}
	maxAnswers := in.MaxAnswers
	if maxAnswers <= 0 {
		maxAnswers = 1
	}
	if maxAnswers > len(in.Options) {
		maxAnswers = len(in.Options)
	}
	return sent(t.SendPoll(ctx, t.account, in.To, in.Question, in.Options, maxAnswers))
}

type reactInput struct {
	Chat      string `json:"chat" jsonschema:"chat JID or phone number the message belongs to"`
	MessageID string `json:"message_id" jsonschema:"id of the message to react to"`
	Emoji     string `json:"emoji" jsonschema:"a single emoji, or an empty string to remove the reaction"`
	FromMe    *bool  `json:"from_me,omitempty" jsonschema:"whether the target message was sent by this account; only needed when it is not in the stored history"`
}

type okOutput struct {
	OK bool `json:"ok"`
}

func (t *tools) react(ctx context.Context, in reactInput) (okOutput, error) {
	if in.MessageID == "" {
		return okOutput{}, errors.New("message_id is required")
	}
	if err := t.React(ctx, t.account, in.Chat, in.MessageID, in.Emoji, in.FromMe); err != nil {
		return okOutput{}, err
	}
	return okOutput{OK: true}, nil
}

type markReadInput struct {
	Chat       string   `json:"chat" jsonschema:"chat JID or phone number"`
	MessageIDs []string `json:"message_ids" jsonschema:"ids of the messages to mark as read"`
}

func (t *tools) markRead(ctx context.Context, in markReadInput) (okOutput, error) {
	if len(in.MessageIDs) == 0 {
		return okOutput{}, errors.New("message_ids is required")
	}
	if err := t.MarkRead(ctx, t.account, in.Chat, in.MessageIDs); err != nil {
		return okOutput{}, err
	}
	return okOutput{OK: true}, nil
}

// ---------------------------------------------------------------- helpers

// chatJID returns the JID under which a chat is stored.
func chatJID(chat string) (string, error) {
	jid, err := wa.ParseRecipient(chat)
	if err != nil {
		return "", errors.New("invalid chat: use a JID from list_chats or a phone number with country code")
	}
	return jid.String(), nil
}

func isGroupJID(jid string) bool {
	return strings.HasSuffix(jid, "@g.us")
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func containsAny(lowerQuery string, values ...string) bool {
	for _, v := range values {
		if strings.Contains(strings.ToLower(v), lowerQuery) {
			return true
		}
	}
	return false
}

func limitOr(limit, def, max int) int {
	if limit <= 0 {
		return def
	}
	if limit > max {
		return max
	}
	return limit
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
