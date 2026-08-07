package builtin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTestPNG renders a deterministic gradient PNG of the given size.
func writeTestPNG(t *testing.T, dir, name string, w, h int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		c := color.RGBA{uint8(x % 256), 200, 128, 255}
		for y := 0; y < h; y++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png encode: %v", err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func readImageTool(t *testing.T, path string) (string, []string, error) {
	t.Helper()
	r := readFile{workDir: t.TempDir()}
	return r.ExecuteWithImages(context.Background(), json.RawMessage(`{"path":"`+path+`"}`))
}

func decodeDataURL(t *testing.T, dataURL string) (image.Config, string) {
	t.Helper()
	rest, ok := strings.CutPrefix(dataURL, "data:")
	if !ok {
		t.Fatalf("not a data URL: %.40s", dataURL)
	}
	mime, b64, _ := strings.Cut(rest, ";")
	b64, _ = strings.CutPrefix(b64, "base64,")
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decode config: %v", err)
	}
	return cfg, mime
}

// An oversized screenshot must be clamped to 1568px and re-encoded as JPEG,
// with the placeholder + compression note in the text.
func TestReadFileImageOversizedCompressed(t *testing.T) {
	dir := t.TempDir()
	path := writeTestPNG(t, dir, "big.png", 2000, 1500)

	text, images, err := readImageTool(t, path)
	if err != nil {
		t.Fatalf("ExecuteWithImages: %v", err)
	}
	if len(images) != 1 {
		t.Fatalf("expected 1 image, got %d", len(images))
	}
	cfg, mime := decodeDataURL(t, images[0])
	if mime != "image/jpeg" {
		t.Fatalf("oversized raster should become JPEG, got %s", mime)
	}
	if cfg.Width > 1568 || cfg.Height > 1568 {
		t.Fatalf("image not clamped: %dx%d", cfg.Width, cfg.Height)
	}
	if !strings.Contains(text, "[image: image/jpeg]") {
		t.Fatalf("placeholder missing in text: %.100s", text)
	}
	if !strings.Contains(text, "compressed from") {
		t.Fatalf("compression note missing in text: %.100s", text)
	}
}

// A small image stays intact (re-encode would inflate it) but still reports a
// single data-URL image and the placeholder.
func TestReadFileImageWithinBudget(t *testing.T) {
	dir := t.TempDir()
	path := writeTestPNG(t, dir, "small.png", 800, 600)

	text, images, err := readImageTool(t, path)
	if err != nil {
		t.Fatalf("ExecuteWithImages: %v", err)
	}
	if len(images) != 1 {
		t.Fatalf("expected 1 image, got %d", len(images))
	}
	if _, mime := decodeDataURL(t, images[0]); !strings.HasPrefix(mime, "image/") {
		t.Fatalf("unexpected mime %s", mime)
	}
	if !strings.Contains(text, "[image: ") {
		t.Fatalf("placeholder missing in text: %.100s", text)
	}
}

// Text files must be byte-identical to the plain Execute path: no images,
// line-numbered output.
func TestReadFileImageNotForText(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(path, []byte("hello\nworld\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	text, images, err := readImageTool(t, path)
	if err != nil {
		t.Fatalf("ExecuteWithImages: %v", err)
	}
	if len(images) != 0 {
		t.Fatalf("text file must not produce images, got %d", len(images))
	}
	if !strings.Contains(text, "1→hello") {
		t.Fatalf("text pipeline regressed: %.100s", text)
	}
}

// Env knobs: quality, langs, and the OCR kill-switch.
func TestReadFileImageEnvKnobs(t *testing.T) {
	t.Setenv(reasonixImageQualityEnv, "55")
	if q := imageQuality(); q != 55 {
		t.Fatalf("imageQuality: got %d, want 55", q)
	}
	t.Setenv(reasonixImageQualityEnv, "not-a-number")
	if q := imageQuality(); q != 80 {
		t.Fatalf("imageQuality fallback: got %d, want 80", q)
	}
	t.Setenv(reasonixImageOCRLangsEnv, "eng")
	if l := imageOCRLangs(); l != "eng" {
		t.Fatalf("imageOCRLangs: got %s, want eng", l)
	}
	// Kill-switch short-circuits before touching tesseract.
	t.Setenv(reasonixImageOCREnv, "0")
	if s := readImageOCR(context.Background(), []byte("junk")); s != "" {
		t.Fatalf("OCR should be disabled: %q", s)
	}
}
