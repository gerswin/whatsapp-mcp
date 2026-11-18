package wa

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"syscall"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"

	"github.com/gerswin/whatsapp-mcp/internal/media"
)

// Sent identifies a message that was just delivered to WhatsApp.
type Sent struct {
	MessageID string
	Chat      string
	Timestamp time.Time
}

// deliver sends msg to "to" and records it in the history.
func (m *Manager) deliver(ctx context.Context, client *whatsmeow.Client, account string, to types.JID, msg *waE2E.Message) (Sent, error) {
	resp, err := client.SendMessage(ctx, to, msg)
	if err != nil {
		return Sent{}, fmt.Errorf("WhatsApp did not accept the message: %w", err)
	}
	m.recordSent(client, account, to, resp.ID, resp.Timestamp, msg)
	return Sent{MessageID: resp.ID, Chat: to.String(), Timestamp: resp.Timestamp}, nil
}

// target resolves the account's client and the recipient in one step.
func (m *Manager) target(account, to string) (*whatsmeow.Client, types.JID, error) {
	jid, err := ParseRecipient(to)
	if err != nil {
		return nil, types.JID{}, err
	}
	client, err := m.ready(account)
	if err != nil {
		return nil, types.JID{}, err
	}
	return client, jid, nil
}

func (m *Manager) SendText(ctx context.Context, account, to, text, replyTo string) (Sent, error) {
	client, jid, err := m.target(account, to)
	if err != nil {
		return Sent{}, err
	}

	msg := &waE2E.Message{Conversation: proto.String(text)}
	if replyTo != "" {
		msg = &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text:        proto.String(text),
			ContextInfo: m.quote(client, account, jid, replyTo),
		}}
	}
	return m.deliver(ctx, client, account, jid, msg)
}

// quote builds the reference that makes a message show as a reply. The
// author of the quoted message comes from the stored history; without it we
// assume the other side of a 1:1 chat.
func (m *Manager) quote(client *whatsmeow.Client, account string, chat types.JID, messageID string) *waE2E.ContextInfo {
	participant := chat.String()
	quoted := &waE2E.Message{Conversation: proto.String("")}

	if stored, _ := m.store.Get(account, chat.String(), messageID); stored != nil {
		if stored.FromMe {
			participant = client.Store.GetJID().ToNonAD().String()
		} else if stored.SenderJID != "" {
			participant = stored.SenderJID
		}
		quoted.Conversation = proto.String(stored.Text)
	}

	return &waE2E.ContextInfo{
		StanzaID:      proto.String(messageID),
		Participant:   proto.String(participant),
		QuotedMessage: quoted,
	}
}

// MediaRequest describes a file to send, fetched from a public URL.
type MediaRequest struct {
	To       string
	URL      string
	Type     string // image, video, audio or document
	Caption  string
	Filename string
}

func (m *Manager) SendMedia(ctx context.Context, account string, req MediaRequest) (Sent, error) {
	client, jid, err := m.target(account, req.To)
	if err != nil {
		return Sent{}, err
	}

	data, mime, err := m.fetch(ctx, req.URL)
	if err != nil {
		return Sent{}, err
	}

	var msg *waE2E.Message
	switch req.Type {
	case "image":
		if !strings.HasPrefix(mime, "image/") {
			return Sent{}, fmt.Errorf("that URL is not an image (it is %s)", mime)
		}
		up, err := client.Upload(ctx, data, whatsmeow.MediaImage)
		if err != nil {
			return Sent{}, fmt.Errorf("upload to WhatsApp failed: %w", err)
		}
		thumb, _ := media.ToJPEG(data, 96, 60) // best effort; WhatsApp shows a placeholder without it
		msg = &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
			URL: &up.URL, DirectPath: &up.DirectPath, MediaKey: up.MediaKey,
			FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: &up.FileLength,
			Mimetype: &mime, Caption: optional(req.Caption), JPEGThumbnail: thumb,
		}}

	case "video":
		if !strings.HasPrefix(mime, "video/") {
			return Sent{}, fmt.Errorf("that URL is not a video (it is %s)", mime)
		}
		up, err := client.Upload(ctx, data, whatsmeow.MediaVideo)
		if err != nil {
			return Sent{}, fmt.Errorf("upload to WhatsApp failed: %w", err)
		}
		msg = &waE2E.Message{VideoMessage: &waE2E.VideoMessage{
			URL: &up.URL, DirectPath: &up.DirectPath, MediaKey: up.MediaKey,
			FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: &up.FileLength,
			Mimetype: &mime, Caption: optional(req.Caption),
		}}

	case "audio":
		if !strings.HasPrefix(mime, "audio/") && mime != "application/ogg" {
			return Sent{}, fmt.Errorf("that URL is not an audio file (it is %s)", mime)
		}
		up, err := client.Upload(ctx, data, whatsmeow.MediaAudio)
		if err != nil {
			return Sent{}, fmt.Errorf("upload to WhatsApp failed: %w", err)
		}
		// Only Ogg/Opus can be delivered as a voice note; anything else goes
		// as a regular audio file.
		voice := strings.Contains(mime, "ogg")
		if voice {
			mime = "audio/ogg; codecs=opus"
		}
		msg = &waE2E.Message{AudioMessage: &waE2E.AudioMessage{
			URL: &up.URL, DirectPath: &up.DirectPath, MediaKey: up.MediaKey,
			FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: &up.FileLength,
			Mimetype: &mime, PTT: &voice,
		}}

	case "document":
		up, err := client.Upload(ctx, data, whatsmeow.MediaDocument)
		if err != nil {
			return Sent{}, fmt.Errorf("upload to WhatsApp failed: %w", err)
		}
		name := req.Filename
		if name == "" {
			name = filenameFromURL(req.URL)
		}
		msg = &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
			URL: &up.URL, DirectPath: &up.DirectPath, MediaKey: up.MediaKey,
			FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: &up.FileLength,
			Mimetype: &mime, FileName: &name, Title: &name, Caption: optional(req.Caption),
		}}

	default:
		return Sent{}, errors.New("type must be one of: image, video, audio, document")
	}

	return m.deliver(ctx, client, account, jid, msg)
}

func (m *Manager) SendLocation(ctx context.Context, account, to string, lat, lon float64, name, address string) (Sent, error) {
	client, jid, err := m.target(account, to)
	if err != nil {
		return Sent{}, err
	}
	return m.deliver(ctx, client, account, jid, &waE2E.Message{LocationMessage: &waE2E.LocationMessage{
		DegreesLatitude:  &lat,
		DegreesLongitude: &lon,
		Name:             optional(name),
		Address:          optional(address),
	}})
}

func (m *Manager) SendPoll(ctx context.Context, account, to, question string, options []string, maxAnswers int) (Sent, error) {
	client, jid, err := m.target(account, to)
	if err != nil {
		return Sent{}, err
	}
	return m.deliver(ctx, client, account, jid, client.BuildPollCreation(question, options, maxAnswers))
}

// React adds (or with an empty emoji, removes) a reaction. fromMe overrides
// what the stored history says about who wrote the target message.
func (m *Manager) React(ctx context.Context, account, chat, messageID, emoji string, fromMe *bool) error {
	client, jid, err := m.target(account, chat)
	if err != nil {
		return err
	}

	own := client.Store.GetJID().ToNonAD()
	sender := jid // 1:1 chat: the other side wrote it unless told otherwise
	if stored, _ := m.store.Get(account, jid.String(), messageID); stored != nil {
		if stored.FromMe {
			sender = own
		} else if parsed, err := types.ParseJID(stored.SenderJID); err == nil && jid.Server == types.GroupServer {
			sender = parsed
		}
	}
	if fromMe != nil {
		if *fromMe {
			sender = own
		} else if sender == own {
			sender = jid
		}
	}

	// Reactions are not stored, so this bypasses deliver.
	if _, err := client.SendMessage(ctx, jid, client.BuildReaction(jid, sender, messageID, emoji)); err != nil {
		return fmt.Errorf("WhatsApp did not accept the reaction: %w", err)
	}
	return nil
}

func (m *Manager) MarkRead(ctx context.Context, account, chat string, messageIDs []string) error {
	client, jid, err := m.target(account, chat)
	if err != nil {
		return err
	}

	// Group receipts are addressed per author, so group by sender.
	bySender := map[types.JID][]types.MessageID{}
	for _, id := range messageIDs {
		sender := jid
		if jid.Server == types.GroupServer {
			sender = types.JID{}
			if stored, _ := m.store.Get(account, jid.String(), id); stored != nil {
				if parsed, err := types.ParseJID(stored.SenderJID); err == nil {
					sender = parsed
				}
			}
			if sender.IsEmpty() {
				return fmt.Errorf("message %s is not in the stored history of that group, so it cannot be marked as read", id)
			}
		}
		bySender[sender] = append(bySender[sender], id)
	}

	for sender, ids := range bySender {
		if err := client.MarkRead(ctx, ids, time.Now(), jid, sender); err != nil {
			return fmt.Errorf("WhatsApp did not accept the read receipt: %w", err)
		}
	}
	return nil
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func filenameFromURL(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		if name := path.Base(u.Path); name != "" && name != "." && name != "/" {
			return name
		}
	}
	return "file"
}

// ------------------------------------------------------------- fetching

const maxFetchBytes = 64 << 20

// fetch downloads a file from a public URL. The URL comes from an assistant,
// so anything that is not a public internet address is refused: otherwise a
// prompt could make this server read from the local network.
func (m *Manager) fetch(ctx context.Context, rawURL string) (data []byte, mime string, err error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, "", errors.New("url must be a public http(s) URL")
	}

	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", errors.New("url must be a public http(s) URL")
	}
	resp, err := m.fetchClient().Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("could not download the file: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("could not download the file: the server answered %d", resp.StatusCode)
	}

	data, err = io.ReadAll(io.LimitReader(resp.Body, maxFetchBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("could not download the file: %w", err)
	}
	if len(data) == 0 {
		return nil, "", errors.New("the file at that URL is empty")
	}
	if len(data) > maxFetchBytes {
		return nil, "", errors.New("the file is larger than 64 MB")
	}

	// Trust the content over the header: servers often send octet-stream.
	mime = http.DetectContentType(data)
	if mime == "application/octet-stream" || strings.HasPrefix(mime, "text/plain") {
		if header := strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0]); header != "" {
			mime = header
		}
	}
	if i := strings.Index(mime, ";"); i >= 0 {
		mime = strings.TrimSpace(mime[:i])
	}
	return data, mime, nil
}

func (m *Manager) fetchClient() *http.Client {
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	if !m.allowPrivateFetch {
		// Checked at connect time, on the resolved address, so DNS tricks and
		// redirects cannot get around it.
		dialer.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if ip := net.ParseIP(host); ip == nil || !isPublicIP(ip) {
				return errors.New("the URL does not point to a public internet address")
			}
			return nil
		}
	}
	return &http.Client{
		Transport: &http.Transport{
			DialContext:           dialer.DialContext,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
		},
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			return nil
		},
	}
}

var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

func isPublicIP(ip net.IP) bool {
	return !(ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsInterfaceLocalMulticast() || cgnat.Contains(ip))
}
