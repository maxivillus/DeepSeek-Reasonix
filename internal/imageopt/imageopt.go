// Package imageopt provides shared image optimization for model-bound payloads:
// clamping to a vision-safe dimension budget and JPEG re-encoding. It is used
// both by control (user-attached chat images) and the read_file builtin
// (agent-read screenshots), so the vision budget stays consistent across
// ingestion paths. It is a leaf package (stdlib + golang.org/x/image only).
package imageopt

import (
	"bytes"
	"image"
	_ "image/gif" // register gif decoder
	"image/jpeg"
	"image/png"

	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // register webp decoder
)

// MaxVisionDim caps the longest image side sent to a model. OpenAI and Anthropic
// downscale to roughly this server-side anyway, so a larger upload only wastes
// request bytes and image tokens without adding fidelity.
const MaxVisionDim = 1568

// maxDecodePixels guards against decompression-bomb attachments: a tiny file can
// declare enormous dimensions. Beyond this we skip decoding and send as-is (still
// bounded by the file cap).
const maxDecodePixels = 50_000_000

// CompressForVision downscales an oversized image to MaxVisionDim and re-encodes
// it — PNG/GIF stay lossless (screenshots, text, transparency), JPEG/WebP go to
// JPEG. Best-effort: an undecodable format, a decode/encode failure, or an image
// already within budget returns the original bytes and mime unchanged.
func CompressForVision(raw []byte, mime string) ([]byte, string) {
	data, m, _, _ := compress(raw, mime, 85)
	return data, m
}

// CompressForRead clamps a raster image to MaxVisionDim and re-encodes it for
// agent read results (read_file). Unlike CompressForVision it always tries JPEG
// (quality defaults to 80, overridable) so screenshots shrink hard. Oversized
// images are always downscaled (the pixel/token budget is the point); the
// original is kept only for already-small images whose JPEG re-encode would
// inflate them (small logos, transparency-only PNGs). Returns the payload to
// embed (with its mime), plus the encoded dimensions (0,0 when the image was
// returned untouched after a decode failure).
func CompressForRead(raw []byte, mime string, quality int) (data []byte, outMime string, w, h int) {
	if quality <= 0 {
		quality = 80
	}
	if quality > 100 {
		quality = 100
	}
	switch mime {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
	default:
		return raw, mime, 0, 0 // bmp/tiff/svg: no decoder wired, send original
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || cfg.Width*cfg.Height > maxDecodePixels {
		return raw, mime, 0, 0
	}
	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return raw, mime, 0, 0
	}
	w, h = cfg.Width, cfg.Height
	scaled := false
	if cfg.Width > MaxVisionDim || cfg.Height > MaxVisionDim {
		w, h = scaledDims(cfg.Width, cfg.Height, MaxVisionDim)
		dst := image.NewRGBA(image.Rect(0, 0, w, h))
		xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), xdraw.Over, nil)
		src = dst
		scaled = true
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, src, &jpeg.Options{Quality: quality}); err != nil {
		return raw, mime, 0, 0
	}
	if !scaled && len(buf.Bytes()) >= len(raw) {
		return raw, mime, cfg.Width, cfg.Height // re-encode would inflate — keep original
	}
	return buf.Bytes(), "image/jpeg", w, h
}

// compress is the shared core used by CompressForVision: downscale to
// MaxVisionDim, PNG/GIF stay lossless, JPEG/WebP go to JPEG at the given
// quality. Returns original bytes/mime (w,h = 0) on any failure.
func compress(raw []byte, mime string, quality int) ([]byte, string, int, int) {
	switch mime {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
	default:
		return raw, mime, 0, 0 // bmp/tiff/svg: no decoder wired, send original
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || cfg.Width*cfg.Height > maxDecodePixels {
		return raw, mime, 0, 0
	}
	if cfg.Width <= MaxVisionDim && cfg.Height <= MaxVisionDim {
		return raw, mime, 0, 0 // within budget — no point re-encoding
	}
	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return raw, mime, 0, 0
	}
	w, h := scaledDims(cfg.Width, cfg.Height, MaxVisionDim)
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), xdraw.Over, nil)

	var buf bytes.Buffer
	if mime == "image/png" || mime == "image/gif" {
		if err := png.Encode(&buf, dst); err != nil {
			return raw, mime, 0, 0
		}
		return buf.Bytes(), "image/png", w, h
	}
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: quality}); err != nil {
		return raw, mime, 0, 0
	}
	return buf.Bytes(), "image/jpeg", w, h
}

// scaledDims returns dimensions with the longest side clamped to m, preserving
// aspect ratio (each side at least 1px).
func scaledDims(w, h, m int) (int, int) {
	if w >= h {
		nh := h * m / w
		if nh < 1 {
			nh = 1
		}
		return m, nh
	}
	nw := w * m / h
	if nw < 1 {
		nw = 1
	}
	return nw, m
}
