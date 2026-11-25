// Package mcpserver exposes WhatsApp accounts as a Model Context Protocol
// server (Streamable HTTP) so assistants such as Claude or ChatGPT can read
// chats and send messages.
//
// Every request to /mcp must carry either an account token ("apikey" header
// or "Authorization: Bearer") or an OAuth access token issued by the built-in
// authorization server (see oauth.go). A session only ever sees one account.
package mcpserver

import (
	"context"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gerswin/whatsapp-mcp/internal/store"
	"github.com/gerswin/whatsapp-mcp/internal/wa"
)

const Path = "/mcp"

// statusChat is the chat that holds contacts' status updates, when kept.
const statusChat = "status@broadcast"

// Backend is the WhatsApp side of the tools; *wa.Manager implements it.
type Backend interface {
	Status(account string) wa.AccountStatus
	Contacts(ctx context.Context, account string) ([]wa.Contact, error)
	ContactName(ctx context.Context, account, jid string) string
	CheckNumbers(ctx context.Context, account string, numbers []string) ([]wa.NumberCheck, error)
	Groups(ctx context.Context, account string) ([]wa.Group, error)
	GroupInfo(ctx context.Context, account, group string) (*wa.GroupDetails, error)
	SendText(ctx context.Context, account, to, text, replyTo string) (wa.Sent, error)
	SendMedia(ctx context.Context, account string, req wa.MediaRequest) (wa.Sent, error)
	SendVoiceNote(ctx context.Context, account, to, text, replyTo string) (wa.Sent, error)
	SendLocation(ctx context.Context, account, to string, lat, lon float64, name, address string) (wa.Sent, error)
	SendPoll(ctx context.Context, account, to, question string, options []string, maxAnswers int) (wa.Sent, error)
	React(ctx context.Context, account, chat, messageID, emoji string, fromMe *bool) error
	MarkRead(ctx context.Context, account, chat string, messageIDs []string) error
	Media(ctx context.Context, account, chat, messageID string) (*wa.Media, error)
	Channels(ctx context.Context, account string) ([]wa.Channel, error)
	ChannelMessages(ctx context.Context, account, channel string, limit, before int) ([]wa.ChannelMessage, error)
}

type Config struct {
	Backend  Backend
	Accounts Accounts
	Store    *store.Store
	// OAuthDBPath is where OAuth clients and token hashes are kept.
	OAuthDBPath string
	// PublicURL is the external https origin; when empty it is taken from the
	// proxy/tunnel headers of each request.
	PublicURL string
	Version   string
	// Transcription tells the assistant whether audio can be transcribed.
	Transcription bool
	// VoiceNotes enables send_voice_note (text-to-speech is configured).
	VoiceNotes bool
	// StatusUpdates tells the assistant that status updates are being kept.
	StatusUpdates bool
}

type accountKey struct{}

type endpoint struct {
	Config
	oauth *oauthServer
}

// Register mounts /mcp and the OAuth routes on r. The returned function
// revokes every OAuth grant of an account; call it when an account is
// removed, so a later account with the same name does not inherit them.
func Register(r *gin.Engine, cfg Config) (revoke func(account string) error, err error) {
	oauth, err := openOAuth(cfg.OAuthDBPath, cfg.PublicURL, cfg.Accounts)
	if err != nil {
		return nil, err
	}
	e := &endpoint{Config: cfg, oauth: oauth}

	// A server is built per request (each one is bound to the authenticated
	// account); the cache keeps tool schema reflection a one-time cost.
	schemas := mcp.NewSchemaCache()

	handler := mcp.NewStreamableHTTPHandler(func(req *http.Request) *mcp.Server {
		account, _ := req.Context().Value(accountKey{}).(string)
		if account == "" {
			return nil
		}
		return e.newServer(account, schemas)
	}, &mcp.StreamableHTTPOptions{
		Stateless:    true,
		JSONResponse: true,
		// The SDK rejects requests that reach us over loopback with a public
		// Host header, which is exactly what a same-host tunnel does. That
		// check guards unauthenticated local servers against DNS rebinding;
		// this endpoint requires a token on every call.
		DisableLocalhostProtection: true,
	})

	oauth.routes(r)
	r.Any(Path, e.authenticate, gin.WrapH(handler))
	return oauth.revokeAccount, nil
}

func (e *endpoint) authenticate(c *gin.Context) {
	account := e.resolveAccount(c)
	if account == "" {
		c.Header("WWW-Authenticate", e.oauth.challengeHeader(c))
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "not authorized"})
		return
	}

	c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), accountKey{}, account))
	c.Next()
}

// resolveAccount maps the request credentials to an account name, or "".
func (e *endpoint) resolveAccount(c *gin.Context) string {
	token := strings.TrimSpace(c.GetHeader("apikey"))
	if token == "" {
		if scheme, value, ok := strings.Cut(c.GetHeader("Authorization"), " "); ok && strings.EqualFold(scheme, "Bearer") {
			token = strings.TrimSpace(value)
		}
	}
	if token == "" {
		return ""
	}

	if strings.HasPrefix(token, accessTokenPrefix) {
		account, err := e.oauth.accountForAccessToken(token)
		if err != nil || !e.Accounts.AccountExists(account) {
			return ""
		}
		return account
	}

	account, _ := e.Accounts.AccountByToken(token)
	return account
}

func (e *endpoint) newServer(account string, schemas *mcp.SchemaCache) *mcp.Server {
	instructions := "WhatsApp account \"" + account + "\". " +
		"Recipients (\"to\", \"chat\") accept a phone number in international format (with country code) " +
		"or a WhatsApp JID (…@s.whatsapp.net for people, …@g.us for groups); prefer the JIDs returned by list_chats. " +
		"Sending tools deliver real messages to real people: confirm the recipient and content with the user before sending. " +
		"Chat history covers messages received or sent since the account was linked, plus what WhatsApp synced at that moment. " +
		"Messages carry their reactions, and polls their current results. " +
		"Channels the account follows are not part of the chat history: read them with list_channels and get_channel_messages. " +
		"Messages with has_media can be opened with get_media: images and stickers are returned as pictures"
	if e.Transcription {
		instructions += ", voice notes, audio and video as a transcript. The text of a voice note or audio message, when present, is its automatic transcript and may contain recognition mistakes."
	} else {
		instructions += ". Audio transcription is not configured on this server, so voice notes cannot be read."
	}
	if e.VoiceNotes {
		instructions += " send_voice_note speaks a text with a synthetic voice and delivers it as a voice note; use it only when the user asks for a voice note or audio, not in place of a text message."
	}
	if e.StatusUpdates {
		instructions += " Status updates posted by contacts are kept in the chat \"" + statusChat + "\"; they are not conversations and cannot be replied to from here."
	}

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "whatsapp-mcp",
		Title:   "WhatsApp",
		Version: e.Version,
	}, &mcp.ServerOptions{
		Instructions: instructions,
		SchemaCache:  schemas,
	})

	t := &tools{Backend: e.Backend, store: e.Store, account: account}
	t.register(server, e.VoiceNotes)

	return server
}
