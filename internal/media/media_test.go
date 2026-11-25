package media

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestToJPEGScalesDown(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 400, 200))
	for x := 0; x < 400; x++ {
		src.Set(x, 10, color.NRGBA{R: 255, A: 255})
	}
	var in bytes.Buffer
	if err := png.Encode(&in, src); err != nil {
		t.Fatal(err)
	}

	out, err := ToJPEG(in.Bytes(), 100, 80)
	if err != nil {
		t.Fatalf("ToJPEG: %v", err)
	}
	img, err := jpeg.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("output is not a JPEG: %v", err)
	}
	if b := img.Bounds(); b.Dx() != 100 || b.Dy() != 50 {
		t.Fatalf("size = %dx%d, want 100x50", b.Dx(), b.Dy())
	}
	// Transparent source pixels end up white, not black.
	if r, g, b, _ := img.At(50, 40).RGBA(); r>>8 < 240 || g>>8 < 240 || b>>8 < 240 {
		t.Fatalf("transparent area is not white: %d %d %d", r>>8, g>>8, b>>8)
	}

	if _, err := ToJPEG([]byte("not an image"), 100, 80); err == nil {
		t.Fatal("garbage input should fail")
	}
}

func TestWhisperTranscribe(t *testing.T) {
	var gotAuth, gotModel, gotLang, gotName string
	var gotFile []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/transcriptions" {
			http.NotFound(w, r)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		gotModel = r.FormValue("model")
		gotLang = r.FormValue("language")
		f, hdr, err := r.FormFile("file")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		gotName = hdr.Filename
		gotFile, _ = io.ReadAll(f)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"text":"  hola, llego en diez minutos \n"}`)
	}))
	defer srv.Close()

	w := NewWhisper(srv.URL+"/v1/", "sk-test", "", "es")
	text, err := w.Transcribe(context.Background(), []byte("OggS..."), "audio/ogg; codecs=opus")
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if text != "hola, llego en diez minutos" {
		t.Fatalf("text = %q", text)
	}
	if gotAuth != "Bearer sk-test" || gotModel != "whisper-1" || gotLang != "es" || gotName != "audio.ogg" || string(gotFile) != "OggS..." {
		t.Fatalf("unexpected request: auth=%q model=%q lang=%q name=%q file=%q", gotAuth, gotModel, gotLang, gotName, gotFile)
	}
}

func TestWhisperErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"message":"Incorrect API key provided"}}`)
	}))
	defer srv.Close()

	w := NewWhisper(srv.URL, "bad", "", "")
	if _, err := w.Transcribe(context.Background(), []byte("x"), "audio/ogg"); err == nil || !strings.Contains(err.Error(), "Incorrect API key") {
		t.Fatalf("expected the API error to surface, got %v", err)
	}
	if _, err := w.Transcribe(context.Background(), nil, "audio/ogg"); err == nil {
		t.Fatal("empty audio should fail")
	}
	if _, err := w.Transcribe(context.Background(), make([]byte, MaxTranscribeBytes+1), "audio/ogg"); err == nil {
		t.Fatal("oversized audio should fail")
	}
}

// oggOpus builds the two pages that matter of an Ogg/Opus file: the OpusHead
// header with its pre-skip, and a last page with the final granule position.
func oggOpus(preSkip uint16, granule uint64) []byte {
	page := func(granule uint64, payload []byte) []byte {
		p := append([]byte("OggS"), 0, 0)
		p = binary.LittleEndian.AppendUint64(p, granule)
		p = append(p, make([]byte, 12)...) // serial, sequence, checksum
		p = append(p, 1, byte(len(payload)))
		return append(p, payload...)
	}
	head := append([]byte("OpusHead"), 1, 1)
	head = binary.LittleEndian.AppendUint16(head, preSkip)
	head = append(head, make([]byte, 7)...)
	return append(page(0, head), page(granule, []byte("frames"))...)
}

func TestElevenLabsSpeak(t *testing.T) {
	audio := oggOpus(312, 48000*3+312)
	var gotKey, gotPath, gotFormat string
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey, gotPath, gotFormat = r.Header.Get("xi-api-key"), r.URL.Path, r.URL.Query().Get("output_format")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Write(audio)
	}))
	defer srv.Close()

	e := NewElevenLabs(srv.URL+"/", "xi-test", "voice123", "")
	out, err := e.Speak(context.Background(), "  hola, llego en diez minutos ")
	if err != nil {
		t.Fatalf("Speak: %v", err)
	}
	if !bytes.Equal(out, audio) {
		t.Fatal("the audio was not returned as received")
	}
	if gotKey != "xi-test" || gotPath != "/v1/text-to-speech/voice123" || gotFormat != "opus_48000_32" ||
		gotBody["text"] != "hola, llego en diez minutos" || gotBody["model_id"] != "eleven_multilingual_v2" {
		t.Fatalf("unexpected request: key=%q path=%q format=%q body=%v", gotKey, gotPath, gotFormat, gotBody)
	}

	if _, err := e.Speak(context.Background(), "   "); err == nil {
		t.Fatal("blank text should fail")
	}
	if _, err := e.Speak(context.Background(), strings.Repeat("a", MaxSpeechChars+1)); err == nil {
		t.Fatal("oversized text should fail")
	}
}

func TestElevenLabsErrors(t *testing.T) {
	status, body := http.StatusUnauthorized, `{"detail":{"status":"invalid_api_key","message":"Invalid API key"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	defer srv.Close()
	e := NewElevenLabs(srv.URL, "bad", "voice123", "")

	if _, err := e.Speak(context.Background(), "hola"); err == nil || !strings.Contains(err.Error(), "Invalid API key") {
		t.Fatalf("expected the API error to surface, got %v", err)
	}
	status, body = http.StatusNotFound, `{"detail":"Voice not found"}`
	if _, err := e.Speak(context.Background(), "hola"); err == nil || !strings.Contains(err.Error(), "Voice not found") {
		t.Fatalf("expected the plain detail to surface, got %v", err)
	}
	// Anything WhatsApp would not play as a voice note is refused.
	status, body = http.StatusOK, "ID3 this is an mp3"
	if _, err := e.Speak(context.Background(), "hola"); err == nil || !strings.Contains(err.Error(), "Ogg/Opus") {
		t.Fatalf("expected non-Opus audio to be refused, got %v", err)
	}
}

func TestOggOpusSeconds(t *testing.T) {
	if got := OggOpusSeconds(oggOpus(312, 48000*3+312)); got != 3 {
		t.Fatalf("3 s file = %d", got)
	}
	if got := OggOpusSeconds(oggOpus(312, 48000*3+313)); got != 4 {
		t.Fatalf("a partial second rounds up, got %d", got)
	}
	if got := OggOpusSeconds([]byte("not ogg")); got != 0 {
		t.Fatalf("garbage = %d", got)
	}
}
