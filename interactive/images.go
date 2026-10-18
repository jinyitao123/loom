package interactive

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/gif"
	"image/jpeg"
	"image/png"
)

// Images larger than these limits are scaled down before they are stored and
// sent: providers reject or silently shrink bigger ones, and they cost
// context. MaxFittedImageBytes keeps the base64 form under 5 MB.
const (
	MaxImageSide        = 2000
	MaxFittedImageBytes = 3_750_000
)

// fittedImage is an image after fitImage, with its original size.
type fittedImage struct {
	data, mimeType string // base64 data and media type
	width, height  int    // stored size
	origW, origH   int    // original size
	resized        bool
}

// fitImage scales a base64 image down to MaxImageSide and MaxFittedImageBytes,
// re-encoding PNG and GIF as PNG (or JPEG when PNG stays too large) and JPEG
// as JPEG. Images within the limits, and formats the standard library cannot
// decode (WebP), are returned unchanged.
// fitImage 把 base64 图片缩到 MaxImageSide 与 MaxFittedImageBytes 以内：PNG/GIF 重编为 PNG
// （仍过大时改 JPEG），JPEG 仍为 JPEG；未超限或标准库无法解码的格式（WebP）原样返回。
func fitImage(data, mimeType string) fittedImage {
	out := fittedImage{data: data, mimeType: mimeType}
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return out
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return out
	}
	out.width, out.height, out.origW, out.origH = cfg.Width, cfg.Height, cfg.Width, cfg.Height
	if cfg.Width <= MaxImageSide && cfg.Height <= MaxImageSide && len(raw) <= MaxFittedImageBytes {
		return out
	}
	var src image.Image
	switch format {
	case "png":
		src, err = png.Decode(bytes.NewReader(raw))
	case "jpeg":
		src, err = jpeg.Decode(bytes.NewReader(raw))
	case "gif":
		src, err = gif.Decode(bytes.NewReader(raw))
	default:
		return out
	}
	if err != nil {
		return out
	}
	rgba := image.NewRGBA(image.Rect(0, 0, cfg.Width, cfg.Height))
	draw.Draw(rgba, rgba.Bounds(), src, src.Bounds().Min, draw.Src)

	scale := min(1, float64(MaxImageSide)/float64(cfg.Width), float64(MaxImageSide)/float64(cfg.Height))
	for attempt := 0; attempt < 8; attempt++ {
		w, h := max(1, int(float64(cfg.Width)*scale)), max(1, int(float64(cfg.Height)*scale))
		small := rgba
		if w != cfg.Width || h != cfg.Height {
			small = downscale(rgba, w, h)
		}
		if encoded, mime, ok := encodeWithin(small, format != "jpeg"); ok {
			return fittedImage{data: base64.StdEncoding.EncodeToString(encoded), mimeType: mime,
				width: w, height: h, origW: cfg.Width, origH: cfg.Height, resized: true}
		}
		scale *= 0.75
	}
	return out
}

// encodeWithin encodes img as PNG (when preferred) or JPEG at falling
// quality, returning the first encoding within MaxFittedImageBytes.
func encodeWithin(img *image.RGBA, preferPNG bool) ([]byte, string, bool) {
	var buf bytes.Buffer
	if preferPNG {
		if png.Encode(&buf, img) == nil && buf.Len() <= MaxFittedImageBytes {
			return buf.Bytes(), "image/png", true
		}
	}
	// JPEG has no transparency: composite onto white first.
	flat := image.NewRGBA(img.Bounds())
	draw.Draw(flat, flat.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
	draw.Draw(flat, flat.Bounds(), img, img.Bounds().Min, draw.Over)
	for _, quality := range []int{85, 70, 55} {
		buf.Reset()
		if jpeg.Encode(&buf, flat, &jpeg.Options{Quality: quality}) == nil && buf.Len() <= MaxFittedImageBytes {
			return buf.Bytes(), "image/jpeg", true
		}
	}
	return nil, "", false
}

// downscale shrinks src to w×h by averaging the source pixels each target
// pixel covers.
func downscale(src *image.RGBA, w, h int) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	sw, sh := src.Bounds().Dx(), src.Bounds().Dy()
	for y := 0; y < h; y++ {
		y0, y1 := y*sh/h, max((y+1)*sh/h, y*sh/h+1)
		for x := 0; x < w; x++ {
			x0, x1 := x*sw/w, max((x+1)*sw/w, x*sw/w+1)
			var r, g, b, a, n uint64
			for sy := y0; sy < y1; sy++ {
				row := src.Pix[sy*src.Stride:]
				for sx := x0; sx < x1; sx++ {
					p := row[sx*4 : sx*4+4]
					r, g, b, a = r+uint64(p[0]), g+uint64(p[1]), b+uint64(p[2]), a+uint64(p[3])
					n++
				}
			}
			d := dst.Pix[y*dst.Stride+x*4:]
			d[0], d[1], d[2], d[3] = uint8(r/n), uint8(g/n), uint8(b/n), uint8(a/n)
		}
	}
	return dst
}

// fitImageBlocks fits every image block, adding a note after each resized
// one so the model can map positions back to the original.
func fitImageBlocks(blocks []Block) []Block {
	var out []Block
	for _, block := range blocks {
		if block.Type != "image" || block.Data == "" {
			out = append(out, block)
			continue
		}
		fitted := fitImage(block.Data, block.MimeType)
		block.Data, block.MimeType = fitted.data, fitted.mimeType
		out = append(out, block)
		if fitted.resized {
			out = append(out, Block{Type: "text", Text: fmt.Sprintf("[Image: original %dx%d, displayed at %dx%d. Multiply coordinates by %.2f to map to the original image.]",
				fitted.origW, fitted.origH, fitted.width, fitted.height, float64(fitted.origW)/float64(fitted.width))})
		}
	}
	return out
}
