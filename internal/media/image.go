package media

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"

	// Decoders for everything WhatsApp sends as an image or sticker.
	_ "image/gif"
	_ "image/png"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

// maxPixels guards against decompression bombs before decoding.
const maxPixels = 64 << 20

// ToJPEG decodes an image (JPEG, PNG, GIF or WebP), scales it down so its
// longest edge is at most maxEdge pixels and re-encodes it as JPEG.
// Transparent areas become white.
func ToJPEG(data []byte, maxEdge, quality int) ([]byte, error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("unsupported image format: %w", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width*cfg.Height > maxPixels {
		return nil, fmt.Errorf("image is too large to process (%dx%d)", cfg.Width, cfg.Height)
	}

	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("could not decode %s image: %w", format, err)
	}

	w, h := src.Bounds().Dx(), src.Bounds().Dy()
	if longest := max(w, h); longest > maxEdge {
		w = max(1, w*maxEdge/longest)
		h = max(1, h*maxEdge/longest)
	}

	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(dst, dst.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Over, nil)

	var out bytes.Buffer
	if err := jpeg.Encode(&out, dst, &jpeg.Options{Quality: quality}); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
