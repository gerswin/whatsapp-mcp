package media

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// MaxSpeechChars caps the text of one voice note. ElevenLabs bills per
// character, so this also bounds what a single tool call can spend.
const MaxSpeechChars = 5000

// maxSpeechBytes bounds the audio read back from the API.
const maxSpeechBytes = 32 << 20

// ElevenLabs turns text into speech with the ElevenLabs API
// (POST {BaseURL}/v1/text-to-speech/{VoiceID}). It asks for Ogg/Opus, the
// only format WhatsApp plays as a voice note.
type ElevenLabs struct {
	BaseURL string // e.g. https://api.elevenlabs.io
	APIKey  string
	VoiceID string
	Model   string // e.g. eleven_multilingual_v2
	HTTP    *http.Client
}

func NewElevenLabs(baseURL, apiKey, voiceID, model string) *ElevenLabs {
	if baseURL == "" {
		baseURL = "https://api.elevenlabs.io"
	}
	if model == "" {
		model = "eleven_multilingual_v2"
	}
	return &ElevenLabs{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		VoiceID: voiceID,
		Model:   model,
		HTTP:    &http.Client{Timeout: 3 * time.Minute},
	}
}

// Speak returns text read aloud as an Ogg/Opus file.
func (e *ElevenLabs) Speak(ctx context.Context, text string) ([]byte, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, errors.New("there is no text to speak")
	}
	if n := len([]rune(text)); n > MaxSpeechChars {
		return nil, fmt.Errorf("the text is too long for a voice note (%d characters, the limit is %d)", n, MaxSpeechChars)
	}

	payload, err := json.Marshal(map[string]string{"text": text, "model_id": e.Model})
	if err != nil {
		return nil, err
	}
	endpoint := e.BaseURL + "/v1/text-to-speech/" + url.PathEscape(e.VoiceID) + "?output_format=opus_48000_32"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("xi-api-key", e.APIKey)

	resp, err := e.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("text-to-speech request failed: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxSpeechBytes+1))
	if err != nil {
		return nil, fmt.Errorf("text-to-speech request failed: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("text-to-speech service returned %d: %s", resp.StatusCode, elevenLabsError(raw))
	}
	if len(raw) > maxSpeechBytes {
		return nil, errors.New("the generated audio is too large")
	}
	if !IsOggOpus(raw) {
		return nil, errors.New("the text-to-speech service did not return Ogg/Opus audio, which WhatsApp needs for voice notes")
	}
	return raw, nil
}

// elevenLabsError extracts the message of an error body, which is either
// {"detail": {"message": "..."}} or {"detail": "..."}.
func elevenLabsError(raw []byte) string {
	var body struct {
		Detail json.RawMessage `json:"detail"`
	}
	if json.Unmarshal(raw, &body) == nil && len(body.Detail) > 0 {
		var detail struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(body.Detail, &detail) == nil && detail.Message != "" {
			return detail.Message
		}
		var plain string
		if json.Unmarshal(body.Detail, &plain) == nil && plain != "" {
			return plain
		}
	}
	msg := strings.TrimSpace(string(raw))
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return msg
}

// IsOggOpus reports whether data is an Opus stream in an Ogg container.
func IsOggOpus(data []byte) bool {
	head := data
	if len(head) > 256 {
		head = head[:256]
	}
	return bytes.HasPrefix(data, []byte("OggS")) && bytes.Contains(head, []byte("OpusHead"))
}

// OggOpusSeconds returns the length of an Ogg/Opus file, rounded up, or 0
// when it cannot be told. The granule position of the last page counts
// samples at 48 kHz, whatever the rate the audio was encoded from.
func OggOpusSeconds(data []byte) uint32 {
	last := bytes.LastIndex(data, []byte("OggS"))
	if last < 0 || len(data) < last+14 {
		return 0
	}
	granule := binary.LittleEndian.Uint64(data[last+6 : last+14])
	if granule == 0 || granule == ^uint64(0) {
		return 0
	}
	// The samples the decoder must discard at the start are in OpusHead.
	if head := bytes.Index(data, []byte("OpusHead")); head >= 0 && len(data) >= head+12 {
		if skip := uint64(binary.LittleEndian.Uint16(data[head+10 : head+12])); skip < granule {
			granule -= skip
		}
	}
	return uint32((granule + 47999) / 48000)
}
