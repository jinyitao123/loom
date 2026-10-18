package interactive

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"math/rand"
	"strings"
	"testing"

	"github.com/jinyitao123/loom/contract"
)

func gradientPNG(t *testing.T, w, h int, pixel func(x, y int) color.RGBA) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, pixel(x, y))
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func decodedSize(t *testing.T, data string) (int, int, string) {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		t.Fatal(err)
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Width, cfg.Height, format
}

func TestFitImageScalesLargeImagesDown(t *testing.T) {
	gradient := func(x, y int) color.RGBA { return color.RGBA{uint8(x), uint8(y), 128, 255} }
	big := gradientPNG(t, 3000, 1500, gradient)
	fitted := fitImage(big, "image/png")
	if !fitted.resized || fitted.mimeType != "image/png" || fitted.width != 2000 || fitted.height != 1000 {
		t.Fatalf("fitted = %+v", fitted)
	}
	if w, h, format := decodedSize(t, fitted.data); w != 2000 || h != 1000 || format != "png" {
		t.Fatalf("stored image = %dx%d %s", w, h, format)
	}

	small := gradientPNG(t, 40, 30, gradient)
	if f := fitImage(small, "image/png"); f.resized || f.data != small {
		t.Fatalf("a small image must stay as it is: %+v", f.resized)
	}
	if f := fitImage("not base64!", "image/png"); f.resized || f.data != "not base64!" {
		t.Fatal("undecodable data must stay as it is")
	}
}

func TestFitImageFallsBackToJPEGWhenPNGStaysTooLarge(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	noise := gradientPNG(t, 1300, 1300, func(int, int) color.RGBA {
		return color.RGBA{uint8(rng.Intn(256)), uint8(rng.Intn(256)), uint8(rng.Intn(256)), 255}
	})
	fitted := fitImage(noise, "image/png")
	if !fitted.resized || fitted.mimeType != "image/jpeg" || len(fitted.data)*3/4 > MaxFittedImageBytes {
		t.Fatalf("fitted = %s, %d bytes", fitted.mimeType, len(fitted.data)*3/4)
	}
	if _, _, format := decodedSize(t, fitted.data); format != "jpeg" {
		t.Fatalf("format = %s", format)
	}
}

func TestDownscaleAveragesCoveredPixels(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 2, 2))
	src.SetRGBA(0, 0, color.RGBA{0, 0, 0, 255})
	src.SetRGBA(1, 0, color.RGBA{200, 0, 0, 255})
	src.SetRGBA(0, 1, color.RGBA{0, 100, 0, 255})
	src.SetRGBA(1, 1, color.RGBA{0, 0, 40, 255})
	if got := downscale(src, 1, 1).RGBAAt(0, 0); got != (color.RGBA{50, 25, 10, 255}) {
		t.Fatalf("average = %v", got)
	}
}

func TestToolResultImagesAreFittedBeforeTheModelSeesThem(t *testing.T) {
	big := gradientPNG(t, 2400, 600, func(x, y int) color.RGBA { return color.RGBA{uint8(x), 0, 0, 255} })
	result := &contract.ToolResult{Content: "screenshot", Parts: []contract.ContentPart{{Type: "text", Text: "screenshot"}, {Type: "image", MediaType: "image/png", Data: big}}}
	fitResultImages(result)
	if len(result.Parts) != 3 || result.Parts[1].Type != "image" || !strings.Contains(result.Parts[2].Text, "original 2400x600, displayed at 2000x500") {
		t.Fatalf("parts = %d, note %q", len(result.Parts), result.Parts[len(result.Parts)-1].Text)
	}
	if w, _, _ := decodedSize(t, result.Parts[1].Data); w != 2000 {
		t.Fatalf("width = %d", w)
	}
}
