package imageproc

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

func makeSolidPNG(w, h int, c color.Color) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

func makeTransparentPNG(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	// Leave fully transparent (alpha=0).
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

func makeJPEG(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: 255, G: 100, B: 50, A: 255})
		}
	}
	var buf bytes.Buffer
	_ = jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90})
	return buf.Bytes()
}

func TestOptimize_OpaquePNGBecomesJPEG(t *testing.T) {
	raw := makeSolidPNG(800, 600, color.RGBA{R: 200, G: 200, B: 200, A: 255})
	result, err := Optimize(raw, DefaultOptions)
	if err != nil {
		t.Fatalf("Optimize() error = %v", err)
	}
	if result.MimeType != "image/jpeg" {
		t.Errorf("opaque image should re-encode to JPEG, got %s", result.MimeType)
	}
	if result.Width != 800 || result.Height != 600 {
		t.Errorf("dimensions changed unexpectedly: %dx%d", result.Width, result.Height)
	}
	if result.Base64 == "" {
		t.Error("expected non-empty base64")
	}
}

func TestOptimize_TransparentPNGStaysPNG(t *testing.T) {
	raw := makeTransparentPNG(400, 300)
	result, err := Optimize(raw, DefaultOptions)
	if err != nil {
		t.Fatalf("Optimize() error = %v", err)
	}
	if result.MimeType != "image/png" {
		t.Errorf("transparent image should stay PNG, got %s", result.MimeType)
	}
}

func TestOptimize_DownscalesLargeImage(t *testing.T) {
	raw := makeSolidPNG(3000, 2000, color.RGBA{R: 100, G: 100, B: 100, A: 255})
	result, err := Optimize(raw, DefaultOptions)
	if err != nil {
		t.Fatalf("Optimize() error = %v", err)
	}
	if result.Width > DefaultOptions.MaxWidth {
		t.Errorf("width %d exceeds max %d", result.Width, DefaultOptions.MaxWidth)
	}
	if result.Height > DefaultOptions.MaxHeight {
		t.Errorf("height %d exceeds max %d", result.Height, DefaultOptions.MaxHeight)
	}
}

func TestOptimize_DoesNotUpscale(t *testing.T) {
	raw := makeSolidPNG(100, 80, color.White)
	result, err := Optimize(raw, DefaultOptions)
	if err != nil {
		t.Fatalf("Optimize() error = %v", err)
	}
	if result.Width != 100 || result.Height != 80 {
		t.Errorf("small image should not be upscaled: got %dx%d", result.Width, result.Height)
	}
}

func TestOptimize_JPEGInput(t *testing.T) {
	raw := makeJPEG(500, 400)
	result, err := Optimize(raw, DefaultOptions)
	if err != nil {
		t.Fatalf("Optimize() error = %v", err)
	}
	if result.MimeType != "image/jpeg" {
		t.Errorf("JPEG input should produce JPEG output, got %s", result.MimeType)
	}
}

func TestOptimize_EmptyInput(t *testing.T) {
	_, err := Optimize(nil, DefaultOptions)
	if err == nil {
		t.Error("expected error for empty input")
	}
}

func TestOptimize_InvalidInput(t *testing.T) {
	_, err := Optimize([]byte("not an image"), DefaultOptions)
	if err == nil {
		t.Error("expected error for invalid image data")
	}
}

func TestOptimize_ReducesSize(t *testing.T) {
	// A large solid-color PNG should compress dramatically as JPEG.
	raw := makeSolidPNG(2000, 1500, color.RGBA{R: 128, G: 64, B: 200, A: 255})
	result, err := Optimize(raw, DefaultOptions)
	if err != nil {
		t.Fatalf("Optimize() error = %v", err)
	}
	// The original PNG is ~12MB for 2000x1500 RGBA. Optimized JPEG should be
	// far smaller (and also downscaled to ≤1568 wide).
	if result.Bytes > 500_000 {
		t.Errorf("optimized image too large: %d bytes (expected <500KB)", result.Bytes)
	}
}

func TestOptimize_ResizedOpaqueImageBecomesJPEG(t *testing.T) {
	// Regression test: after resize, the alpha check must use the resized
	// image's bounds, not the original. A large opaque image that gets
	// downscaled should still be detected as opaque → JPEG output.
	raw := makeSolidPNG(3000, 2000, color.RGBA{R: 100, G: 150, B: 200, A: 255})
	result, err := Optimize(raw, DefaultOptions)
	if err != nil {
		t.Fatalf("Optimize() error = %v", err)
	}
	if result.MimeType != "image/jpeg" {
		t.Errorf("resized opaque image should be JPEG, got %s (alpha detection bug)", result.MimeType)
	}
	// Verify it was actually resized.
	if result.Width > DefaultOptions.MaxWidth {
		t.Errorf("width %d exceeds max %d", result.Width, DefaultOptions.MaxWidth)
	}
}

func TestSniffFormat(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want string
	}{
		{"png", makeSolidPNG(10, 10, color.White), "image/png"},
		{"jpeg", makeJPEG(10, 10), "image/jpeg"},
		{"empty", []byte{}, ""},
		{"garbage", []byte("hello world"), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sniffFormat(tt.data); got != tt.want {
				t.Errorf("sniffFormat() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEstimateTiles(t *testing.T) {
	if got := EstimateTiles(1000, 800, "low"); got != 1 {
		t.Errorf("low detail should always be 1 tile, got %d", got)
	}
	// 1000x800 at high detail: ceil(1000/512)=2, ceil(800/512)=2 → 4+1=5
	if got := EstimateTiles(1000, 800, "high"); got != 5 {
		t.Errorf("high detail 1000x800 = %d tiles, want 5", got)
	}
}

func TestEstimateTilesToTokens(t *testing.T) {
	if got := EstimateTilesToTokens(1, "low"); got != 85 {
		t.Errorf("low detail tokens = %d, want 85", got)
	}
	// 5 tiles high: 85*5 + 85 = 510
	if got := EstimateTilesToTokens(5, "high"); got != 510 {
		t.Errorf("5 tile tokens = %d, want 510", got)
	}
}
