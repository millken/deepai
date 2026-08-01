// Package imageproc provides image optimization for LLM vision inputs.
//
// The goal is to minimize token cost while preserving enough visual fidelity
// for the model to understand UI layouts, diagrams, and screenshots. The
// pipeline mirrors the engineering recommendations from production AI-agent
// setups:
//
//   - Cap the longest edge at 1568px (the single-tile ceiling for both
//     OpenAI and Anthropic vision APIs).
//   - Re-encode as JPEG quality 82 when the image has no alpha channel
//     (5-10× smaller than PNG with negligible understanding loss).
//   - Preserve PNG for images with transparency (alpha matters for UI).
package imageproc

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"strings"

	"golang.org/x/image/draw"
	"golang.org/x/image/webp"
)

// DefaultOptions are the recommended optimization parameters for agent vision.
// detail="low" maps to a single ~170-token tile on OpenAI, versus ~1000+ for
// "high".
var DefaultOptions = Options{
	MaxWidth:    1568,
	MaxHeight:   1568,
	JPEGQuality: 82,
	Detail:      "low",
}

// Options controls the image optimization pipeline.
type Options struct {
	// MaxWidth is the maximum width in pixels. Images wider than this are
	// proportionally downscaled. Zero means no width cap.
	MaxWidth int

	// MaxHeight is the maximum height in pixels. Images taller than this are
	// proportionally downscaled. Zero means no height cap.
	MaxHeight int

	// JPEGQuality (1-100) controls JPEG re-encoding quality. Only used when
	// the image has no alpha channel.
	JPEGQuality int

	// Detail is the OpenAI vision detail level ("low", "auto", "high").
	// Passed through to the provider; "low" uses a single tile (~170 tokens).
	Detail string
}

// Result holds the optimized image data and metadata.
type Result struct {
	MimeType string // "image/jpeg", "image/png", "image/webp", "image/gif"
	Base64   string // base64-encoded optimized image bytes
	Width    int    // output dimensions
	Height   int
	Bytes    int // size of decoded base64 data (for UI display)
}

// Optimize reads raw image bytes, decodes, optionally resizes, and re-encodes
// to minimize size. It returns the optimized result.
//
// Supported input formats: PNG, JPEG, GIF, WebP.
// The output format is JPEG (quality opts.JPEGQuality) for opaque images, or
// PNG for images with an alpha channel.
func Optimize(raw []byte, opts Options) (Result, error) {
	if len(raw) == 0 {
		return Result{}, fmt.Errorf("empty image data")
	}
	if opts.JPEGQuality <= 0 || opts.JPEGQuality > 100 {
		opts.JPEGQuality = 82
	}

	img, format, err := decode(raw)
	if err != nil {
		return Result{}, fmt.Errorf("decode image: %w", err)
	}

	bounds := img.Bounds()
	srcW := bounds.Dx()
	srcH := bounds.Dy()

	resized, rw, rh := maybeResize(img, bounds, opts)
	img = resized
	// Use the (possibly resized) image's own bounds for alpha detection —
	// maybeResize returns a new image with different dimensions.
	actualBounds := img.Bounds()

	// GIF: preserve as-is (re-encoding would lose animation).
	if format == "gif" {
		return encodeResult(raw, "image/gif", srcW, srcH)
	}

	hasAlpha := hasAlphaChannel(img, actualBounds)

	var buf bytes.Buffer
	mimeType := "image/jpeg"
	if hasAlpha {
		mimeType = "image/png"
		if err := png.Encode(&buf, img); err != nil {
			return Result{}, fmt.Errorf("encode png: %w", err)
		}
	} else {
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: opts.JPEGQuality}); err != nil {
			return Result{}, fmt.Errorf("encode jpeg: %w", err)
		}
	}

	b64 := base64.StdEncoding.EncodeToString(buf.Bytes())
	return Result{
		MimeType: mimeType,
		Base64:   b64,
		Width:    rw,
		Height:   rh,
		Bytes:    buf.Len(),
	}, nil
}

// decode dispatches to the correct decoder based on format magic bytes.
func decode(raw []byte) (image.Image, string, error) {
	contentType := sniffFormat(raw)
	switch contentType {
	case "image/png":
		img, err := png.Decode(bytes.NewReader(raw))
		return img, "png", err
	case "image/jpeg":
		img, err := jpeg.Decode(bytes.NewReader(raw))
		return img, "jpeg", err
	case "image/gif":
		img, err := gif.Decode(bytes.NewReader(raw))
		return img, "gif", err
	case "image/webp":
		img, err := webp.Decode(bytes.NewReader(raw))
		return img, "webp", err
	default:
		// Fallback: let image.Decode try all registered formats.
		img, format, err := image.Decode(bytes.NewReader(raw))
		return img, format, err
	}
}

// sniffFormat detects the image format from magic bytes without full decode.
func sniffFormat(raw []byte) string {
	if len(raw) >= 8 && bytes.Equal(raw[0:8], []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}) {
		return "image/png"
	}
	if len(raw) >= 3 && raw[0] == 0xFF && raw[1] == 0xD8 && raw[2] == 0xFF {
		return "image/jpeg"
	}
	if len(raw) >= 6 && (bytes.Equal(raw[0:6], []byte("GIF87a")) || bytes.Equal(raw[0:6], []byte("GIF89a"))) {
		return "image/gif"
	}
	if len(raw) >= 12 && bytes.Equal(raw[0:4], []byte("RIFF")) && bytes.Equal(raw[8:12], []byte("WEBP")) {
		return "image/webp"
	}
	return ""
}

// maybeResize downscales the image if it exceeds the configured maximum
// dimensions. It never upscales. Returns the (possibly same) image and the
// final dimensions.
func maybeResize(img image.Image, bounds image.Rectangle, opts Options) (image.Image, int, int) {
	srcW := bounds.Dx()
	srcH := bounds.Dy()

	scale := 1.0
	if opts.MaxWidth > 0 && srcW > opts.MaxWidth {
		if s := float64(opts.MaxWidth) / float64(srcW); s < scale {
			scale = s
		}
	}
	if opts.MaxHeight > 0 && srcH > opts.MaxHeight {
		if s := float64(opts.MaxHeight) / float64(srcH); s < scale {
			scale = s
		}
	}

	if scale >= 1.0 {
		return img, srcW, srcH
	}

	dstW := max(1, int(float64(srcW)*scale))
	dstH := max(1, int(float64(srcH)*scale))
	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, bounds, draw.Over, nil)
	return dst, dstW, dstH
}

// hasAlphaChannel reports whether the image uses any non-opaque pixels.
func hasAlphaChannel(img image.Image, bounds image.Rectangle) bool {
	// Sample a subset of pixels for performance on large images.
	stepX := max(1, bounds.Dx()/256)
	stepY := max(1, bounds.Dy()/256)
	for y := bounds.Min.Y; y < bounds.Max.Y; y += stepY {
		for x := bounds.Min.X; x < bounds.Max.X; x += stepX {
			if _, _, _, a := img.At(x, y).RGBA(); a != 0xFFFF {
				return true
			}
		}
	}
	return false
}

func encodeResult(raw []byte, mimeType string, w, h int) (Result, error) {
	return Result{
		MimeType: mimeType,
		Base64:   base64.StdEncoding.EncodeToString(raw),
		Width:    w,
		Height:   h,
		Bytes:    len(raw),
	}, nil
}

// EstimateTiles estimates the number of 512×512 vision tiles an image will
// consume. This is used for UI display of approximate token cost.
//
// With detail="low", the API always uses exactly 1 tile regardless of size.
// With detail="high", tiles = ceil(w/512) * ceil(h/512) + 1 (overview tile).
func EstimateTiles(width, height int, detail string) int {
	if strings.ToLower(detail) == "low" {
		return 1
	}
	tiles := ((width + 511) / 512) * ((height + 511) / 512)
	if tiles == 0 {
		tiles = 1
	}
	return tiles + 1 // +1 overview tile
}

// EstimateTokens gives a rough token estimate for a vision image.
// Low detail: ~170 tokens. High detail: ~85 tokens per tile + 85 base.
func EstimateTilesToTokens(tiles int, detail string) int {
	if strings.ToLower(detail) == "low" {
		return 85
	}
	return 85*tiles + 85
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
