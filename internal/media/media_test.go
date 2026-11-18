package media

import (
	"bytes"
	"context"
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
