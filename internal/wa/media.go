package wa

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"

	"github.com/gerswin/whatsapp-mcp/internal/media"
)

const (
	// maxDownloadBytes bounds what is pulled into memory from WhatsApp.
	maxDownloadBytes = 64 << 20
	// maxTextDocumentBytes bounds text documents returned to the assistant.
	maxTextDocumentBytes = 256 << 10
	// imageMaxEdge keeps images within what assistants accept without
	// downscaling them again.
	imageMaxEdge = 1568
)

// Media is the readable form of a message's file: an image to look at, or
// text (a transcript, or the content of a text document).
type Media struct {
	// Kind is the stored message type: image, sticker, voice_note, audio,
	// video, video_note or document.
	Kind     string
	Filename string
	// Image is a JPEG, set for images and stickers.
	Image []byte
	// Text is the transcript of an audio/video or the content of a text file.
	Text string
}

// Media fetches the file of a stored message from WhatsApp and returns it in
// a form an assistant can use.
func (m *Manager) Media(ctx context.Context, account, chat, messageID string) (*Media, error) {
	jid, err := ParseRecipient(chat)
	if err != nil {
		return nil, err
	}
	chatJID := jid.String()

	stored, err := m.store.Get(account, chatJID, messageID)
	if err != nil {
		return nil, err
	}
	if stored == nil {
		return nil, errors.New("that message is not in the stored history of this chat")
	}
	if stored.Deleted {
		return nil, errors.New("that message was deleted by its sender")
	}

	raw, err := m.store.Media(account, chatJID, messageID)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("that message (%s) has no file to read", stored.Type)
	}
	var ref waE2E.Message
	if err := proto.Unmarshal(raw, &ref); err != nil {
		return nil, errors.New("the stored reference to that file is damaged")
	}

	out := &Media{Kind: stored.Type}

	switch {
	case ref.GetAudioMessage() != nil:
		// The transcript of an audio is cached as the message text.
		if stored.Text != "" {
			out.Text = stored.Text
			return out, nil
		}
		text, err := m.transcribe(ctx, account, &ref, ref.GetAudioMessage().GetMimetype(), ref.GetAudioMessage().GetFileLength())
		if err != nil {
			return nil, err
		}
		if text != "" {
			_ = m.store.SetTranscript(account, chatJID, messageID, text)
		}
		out.Text = text
		return out, nil

	case ref.GetImageMessage() != nil, ref.GetStickerMessage() != nil:
		data, err := m.download(ctx, account, &ref, mediaLength(&ref))
		if err != nil {
			return nil, err
		}
		if out.Image, err = media.ToJPEG(data, imageMaxEdge, 82); err != nil {
			return nil, err
		}
		return out, nil

	case ref.GetVideoMessage() != nil:
		text, err := m.transcribe(ctx, account, &ref, "video/mp4", ref.GetVideoMessage().GetFileLength())
		if err != nil {
			return nil, fmt.Errorf("videos can only be read as a transcript of their audio: %w", err)
		}
		out.Text = text
		return out, nil

	case ref.GetDocumentMessage() != nil:
		doc := ref.GetDocumentMessage()
		out.Filename = doc.GetFileName()
		mime := strings.ToLower(doc.GetMimetype())

		switch {
		case strings.HasPrefix(mime, "image/"):
			data, err := m.download(ctx, account, &ref, doc.GetFileLength())
			if err != nil {
				return nil, err
			}
			if out.Image, err = media.ToJPEG(data, imageMaxEdge, 82); err != nil {
				return nil, err
			}
			return out, nil

		case strings.HasPrefix(mime, "audio/"):
			text, err := m.transcribe(ctx, account, &ref, mime, doc.GetFileLength())
			if err != nil {
				return nil, err
			}
			out.Text = text
			return out, nil

		case isTextMime(mime):
			if doc.GetFileLength() > maxTextDocumentBytes {
				return nil, fmt.Errorf("that text file is too large to read (%d KB)", doc.GetFileLength()>>10)
			}
			data, err := m.download(ctx, account, &ref, doc.GetFileLength())
			if err != nil {
				return nil, err
			}
			if !utf8.Valid(data) {
				return nil, errors.New("that file is not readable text")
			}
			out.Text = string(data)
			return out, nil
		}
		return nil, fmt.Errorf("only image, audio and plain-text documents can be read; this one is %q", doc.GetMimetype())
	}

	return nil, fmt.Errorf("that message (%s) has no file to read", stored.Type)
}

func isTextMime(mime string) bool {
	if strings.HasPrefix(mime, "text/") {
		return true
	}
	for _, m := range []string{"application/json", "application/xml", "application/x-yaml", "application/csv"} {
		if strings.HasPrefix(mime, m) {
			return true
		}
	}
	return false
}

func mediaLength(ref *waE2E.Message) uint64 {
	if img := ref.GetImageMessage(); img != nil {
		return img.GetFileLength()
	}
	return ref.GetStickerMessage().GetFileLength()
}

// download fetches and decrypts the file referenced by ref.
func (m *Manager) download(ctx context.Context, account string, ref *waE2E.Message, length uint64) ([]byte, error) {
	if length > maxDownloadBytes {
		return nil, fmt.Errorf("that file is too large to read (%d MB)", length>>20)
	}
	client, err := m.ready(account)
	if err != nil {
		return nil, err
	}

	data, err := client.DownloadAny(ctx, ref)
	switch {
	case err == nil:
		return data, nil
	case errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith404), errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith410):
		return nil, errors.New("WhatsApp no longer has that file on its servers (it is too old)")
	default:
		return nil, fmt.Errorf("could not download the file from WhatsApp: %w", err)
	}
}

func (m *Manager) transcribe(ctx context.Context, account string, ref *waE2E.Message, mime string, length uint64) (string, error) {
	if m.transcriber == nil {
		return "", errors.New("audio transcription is not configured on this server (WHISPER_API_KEY)")
	}
	if length > media.MaxTranscribeBytes {
		return "", fmt.Errorf("that file is too large to transcribe (%d MB, the limit is 25 MB)", length>>20)
	}
	data, err := m.download(ctx, account, ref, length)
	if err != nil {
		return "", err
	}
	return m.transcriber.Transcribe(ctx, data, mime)
}

func (m *Manager) shouldAutoTranscribe(audio *waE2E.AudioMessage) bool {
	return m.autoEnabled &&
		int(audio.GetSeconds()) <= m.autoMaxSeconds &&
		audio.GetFileLength() <= media.MaxTranscribeBytes
}

// autoTranscribe transcribes an audio that just arrived and stores the text,
// so it shows up in history and search without anyone asking for it.
func (m *Manager) autoTranscribe(client *whatsmeow.Client, account, chatJID, messageID string, msg *waE2E.Message) {
	defer func() {
		if r := recover(); r != nil {
			m.log.Error("panic while transcribing", "account", account, "panic", r)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	audio := msg.GetAudioMessage()
	data, err := client.Download(ctx, audio)
	if err != nil {
		m.log.Warn("could not download audio to transcribe", "account", account, "id", messageID, "err", err)
		return
	}
	text, err := m.transcriber.Transcribe(ctx, data, audio.GetMimetype())
	if err != nil {
		m.log.Warn("could not transcribe audio", "account", account, "id", messageID, "err", err)
		return
	}
	if text == "" {
		return
	}
	if err := m.store.SetTranscript(account, chatJID, messageID, text); err != nil {
		m.log.Error("failed to store transcript", "account", account, "id", messageID, "err", err)
	}
}
