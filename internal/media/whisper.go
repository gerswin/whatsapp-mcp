// Package media has the helpers that make WhatsApp files usable by an
// assistant: speech-to-text for audio and downscaling for images.
package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

// MaxTranscribeBytes is the upload limit of the OpenAI transcription API.
const MaxTranscribeBytes = 25 << 20

// Whisper calls an OpenAI-compatible speech-to-text endpoint
// (POST {BaseURL}/audio/transcriptions).
type Whisper struct {
	BaseURL string // e.g. https://api.openai.com/v1
	APIKey  string
	Model   string // e.g. whisper-1
	// Language is an optional ISO-639-1 hint ("es", "en"); empty auto-detects.
	Language string
	HTTP     *http.Client
}

func NewWhisper(baseURL, apiKey, model, language string) *Whisper {
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	if model == "" {
		model = "whisper-1"
	}
	return &Whisper{
		BaseURL:  strings.TrimRight(baseURL, "/"),
		APIKey:   apiKey,
		Model:    model,
		Language: language,
		HTTP:     &http.Client{Timeout: 3 * time.Minute},
	}
}

func (w *Whisper) Transcribe(ctx context.Context, data []byte, mime string) (string, error) {
	if len(data) == 0 {
		return "", errors.New("the audio is empty")
	}
	if len(data) > MaxTranscribeBytes {
		return "", fmt.Errorf("the file is too large to transcribe (%d MB, the limit is 25 MB)", len(data)>>20)
	}

	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	file, err := form.CreateFormFile("file", "audio."+extensionFor(mime))
	if err != nil {
		return "", err
	}
	if _, err := file.Write(data); err != nil {
		return "", err
	}
	_ = form.WriteField("model", w.Model)
	_ = form.WriteField("response_format", "json")
	if w.Language != "" {
		_ = form.WriteField("language", w.Language)
	}
	if err := form.Close(); err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.BaseURL+"/audio/transcriptions", &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", form.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+w.APIKey)

	resp, err := w.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("transcription request failed: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		var apiErr struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &apiErr)
		msg := apiErr.Error.Message
		if msg == "" {
			msg = strings.TrimSpace(string(raw))
			if len(msg) > 200 {
				msg = msg[:200]
			}
		}
		return "", fmt.Errorf("transcription service returned %d: %s", resp.StatusCode, msg)
	}

	var out struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("unexpected transcription response: %w", err)
	}
	return strings.TrimSpace(out.Text), nil
}

// extensionFor picks the file extension the API uses to recognize the format.
func extensionFor(mime string) string {
	mime = strings.ToLower(mime)
	switch {
	case strings.Contains(mime, "ogg"), strings.Contains(mime, "opus"):
		return "ogg"
	case strings.Contains(mime, "mpeg"), strings.Contains(mime, "mp3"):
		return "mp3"
	case strings.HasPrefix(mime, "video/mp4"):
		return "mp4"
	case strings.Contains(mime, "mp4"), strings.Contains(mime, "m4a"), strings.Contains(mime, "aac"):
		return "m4a"
	case strings.Contains(mime, "wav"):
		return "wav"
	case strings.Contains(mime, "webm"):
		return "webm"
	case strings.Contains(mime, "flac"):
		return "flac"
	}
	return "ogg"
}
