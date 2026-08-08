// Package builtin provides Reasonix's compile-time built-in tools. Each tool
// self-registers via init(); main blank-imports this package to wire them in.
package builtin

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/text/transform"

	fileenc "reasonix/internal/fileutil/encoding"
	"reasonix/internal/imageopt"
	"reasonix/internal/tool"
)

const (
	readFileBinaryPeek   = 8 * 1024   // bytes scanned for NUL before reading further
	readFileDetectSample = 256 * 1024 // bytes sampled for encoding detection before streaming
	// readImageMaxOutputBytes bounds the model-facing text of an image read
	// (header). It sits under the agent's maxToolOutputBytes so the image
	// transcript is never mangled by head+tail truncation.
	readImageMaxOutputBytes = 30 * 1024
)

// Image knobs, mirroring jcode's env-controllable clamp:
//   - REASONIX_IMAGE_JPEG_QUALITY — JPEG quality for re-encoding (default 80)
//   - REASONIX_VISION_URL       — vision-proxy endpoint (POST /vision); если задан,
//     read_file отправляет картинку туда и подмешивает текст/описание в вывод
//     (DeepSeek без vision «видит» содержимое скриншота). Отключить: пустое значение.
//   - REASONIX_VISION_TASK       — describe|document|complex|ocr (default document)
//   - REASONIX_VISION_TIMEOUT    — таймаут vision-вызова, сек (default 120)
const (
	reasonixImageQualityEnv  = "REASONIX_IMAGE_JPEG_QUALITY"
	reasonixVisionURLEnv     = "REASONIX_VISION_URL"
	reasonixVisionTaskEnv    = "REASONIX_VISION_TASK"
	reasonixVisionTimeoutEnv = "REASONIX_VISION_TIMEOUT"
)

func init() { tool.RegisterBuiltin(readFile{}) }

// readFile reads a text file. workDir, when non-empty, is the directory a
// relative path is resolved against (see resolveIn). paths maps session-scoped
// external read aliases to local roots without changing the model-visible tool
// schema. forbidRoots lists directories the tool may not read from (resolved,
// absolute paths).
type readFile struct {
	workDir     string
	paths       *PathResolver
	forbidRoots []string
	// overlay, when non-nil, serves content from the host transport (unsaved
	// editor buffers) before falling back to disk. Consulted only after path
	// resolution and read confinement, and never for external alias paths.
	overlay FileOverlay
}

const (
	readFileDefaultLimit = 2000 // lines returned when limit is unset
)

func (readFile) Name() string { return "read_file" }

func (readFile) Description() string {
	return "Read a text file with optional line offset/limit. Output prefixes each line with its 1-based number (e.g. `   42→...`) so subsequent edit_file calls can target exact lines. Use `offset` and `limit` to page through large files; the tool reports total length and pagination hints in a trailer. Raster images (PNG/JPEG/GIF/WebP) are handled specially: the image is clamped to 1568px and re-encoded as JPEG (quality 80)."
}

func (readFile) Schema() json.RawMessage {
	return json.RawMessage(`{
"type":"object",
"properties":{
  "path":{"type":"string","description":"File path"},
  "offset":{"type":"integer","description":"0-based line offset to start reading from (default 0)","minimum":0},
  "limit":{"type":"integer","description":"Maximum lines to return (default 2000)","minimum":1}
},
"required":["path"]
}`)
}

func (readFile) ReadOnly() bool { return true }

// SnipHint front-loads file content: the most relevant lines are near the top,
// so keep a generous head and a short tail when an old read is shortened.
func (readFile) SnipHint() tool.SnipHint {
	return tool.SnipHint{Head: 120, Tail: 12, HeadChars: 12000, TailChars: 2000}
}

func (r readFile) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		Path   string `json:"path"`
		Offset int    `json:"offset,omitempty"`
		Limit  int    `json:"limit,omitempty"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}
	if p.Path == "" {
		return "", fmt.Errorf("path is required")
	}
	rp := resolveReadablePath(r.workDir, p.Path, r.paths)
	p.Path = rp.Path
	displayPath := rp.DisplayPath
	if confineRead(r.forbidRoots, p.Path) {
		err := &os.PathError{Op: "open", Path: p.Path, Err: os.ErrNotExist}
		if rp.External {
			return "", fmt.Errorf("read %s: %s", displayPath, rp.ErrorText(err))
		}
		return "", err
	}
	if p.Offset < 0 {
		p.Offset = 0
	}
	if p.Limit <= 0 {
		p.Limit = readFileDefaultLimit
	}

	// The host overlay (unsaved editor buffers) wins over the disk when it can
	// serve the path. Content arrives already decoded as text, so the encoding
	// and binary-detection pipeline below applies to the disk fallback only.
	if r.overlay != nil && !rp.External && filepath.IsAbs(p.Path) {
		if content, ok := r.overlay.ReadTextFile(ctx, p.Path); ok {
			return r.scan(strings.NewReader(content), p.Offset, p.Limit)
		}
	}

	// A directory can be os.Open'd but not read as text — catch it up front with
	// an actionable message (and avoid the doubled "read X: read X:" the scanner's
	// error would otherwise produce) so the model switches to the ls tool.
	if info, err := os.Stat(p.Path); err == nil && info.IsDir() {
		return "", fmt.Errorf("%s is a directory, not a file — use the ls tool to list it, or read a specific file inside it", displayPath)
	}

	f, err := os.Open(p.Path)
	if err != nil {
		if rp.External {
			return "", fmt.Errorf("read %s: %s", displayPath, rp.ErrorText(err))
		}
		return "", fmt.Errorf("read %s: %w", displayPath, err)
	}
	defer f.Close()

	// Peek the first 8 KiB to reject binary files cheaply (a NUL byte) before
	// reading further — keeps a multi-GB archive from being slurped just to be
	// discarded.
	peek := make([]byte, readFileBinaryPeek)
	pn, perr := io.ReadFull(f, peek)
	peek = peek[:pn]
	peekEOF := perr != nil // whole file fit in the peek (EOF / ErrUnexpectedEOF)

	// BOM check first: UTF-16 files contain 0x00 for every ASCII character, so a
	// naive NUL check would misidentify them as binary.
	switch fileenc.DetectQuick(peek) {
	case fileenc.UTF16LE, fileenc.UTF16BE:
		// UTF-16 is not self-synchronising and can't be streamed line-by-line, so
		// buffer it fully (these files are rare and usually small).
		rest, rerr := io.ReadAll(f)
		if rerr != nil {
			if rp.External {
				return "", fmt.Errorf("read %s: %s", displayPath, rp.ErrorText(rerr))
			}
			return "", fmt.Errorf("read %s: %w", displayPath, rerr)
		}
		all := append(peek, rest...)
		bom := fileenc.DetectQuick(all)
		return r.scan(bytes.NewReader(fileenc.Decode(all, bom)), p.Offset, p.Limit)
	case fileenc.UTF8BOM:
		// Strip the 3-byte BOM; the content is valid UTF-8 and streams directly.
		body := peek
		if len(body) >= 3 {
			body = body[3:]
		}
		return r.scan(io.MultiReader(bytes.NewReader(body), f), p.Offset, p.Limit)
	}

	// BOM-less UTF-16 (Windows source files) has a NUL for every ASCII char but
	// no BOM, so it reaches here; recognise it by its NUL pattern and decode it
	// rather than rejecting it as binary.
	if k, ok := fileenc.DetectUTF16NoBOM(peek); ok {
		rest, rerr := io.ReadAll(f)
		if rerr != nil {
			if rp.External {
				return "", fmt.Errorf("read %s: %s", displayPath, rp.ErrorText(rerr))
			}
			return "", fmt.Errorf("read %s: %w", displayPath, rerr)
		}
		all := append(peek, rest...)
		return r.scan(bytes.NewReader(fileenc.Decode(all, k)), p.Offset, p.Limit)
	}

	if bytes.IndexByte(peek, 0) >= 0 {
		if rp.External {
			return "", fmt.Errorf("binary file %s (NUL byte detected); not shown by read_file", displayPath)
		}
		return "", fmt.Errorf("binary file %s (NUL byte detected); use `bash hexdump` or another tool", displayPath)
	}

	// Read up to a bounded sample for encoding detection, then stream the rest —
	// so a large text file isn't slurped whole just to return a few lines.
	head := peek
	if !peekEOF {
		more := make([]byte, readFileDetectSample-len(peek))
		mn, merr := io.ReadFull(f, more)
		head = append(peek, more[:mn]...)
		peekEOF = merr != nil
	}

	// Detect from a char-safe slice: when more file follows, trim to the last
	// newline so the sample never ends mid multi-byte sequence (UTF-8 and GB18030
	// are ASCII-transparent, so '\n' is always a clean boundary).
	sample := head
	if !peekEOF {
		if i := bytes.LastIndexByte(head, '\n'); i >= 0 {
			sample = head[:i+1]
		}
	}
	enc, _ := fileenc.Detect(sample)

	src := io.MultiReader(bytes.NewReader(head), f)
	if dec := fileenc.Decoder(enc); dec != nil {
		return r.scan(transform.NewReader(src, dec), p.Offset, p.Limit)
	}
	return r.scan(src, p.Offset, p.Limit)
}

// ExecuteWithImages extends Execute for raster images: it returns the same text
// Execute would, but for image files the text carries the [image: …] placeholder,
// a compression note, and an OCR transcript (so text-only models like DeepSeek
// still see the on-screen content), while the clamped/re-encoded image is
// returned as a data URL for vision-capable providers. Text files fall through
// to Execute's behavior with no images, keeping the text path byte-identical.
func (r readFile) ExecuteWithImages(ctx context.Context, args json.RawMessage) (string, []string, error) {
	var p struct {
		Path   string `json:"path"`
		Offset int    `json:"offset,omitempty"`
		Limit  int    `json:"limit,omitempty"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", nil, fmt.Errorf("invalid args: %w", err)
	}
	if p.Path == "" {
		return "", nil, fmt.Errorf("path is required")
	}
	rp := resolveReadablePath(r.workDir, p.Path, r.paths)
	p.Path = rp.Path
	displayPath := rp.DisplayPath
	if confineRead(r.forbidRoots, p.Path) {
		err := &os.PathError{Op: "open", Path: p.Path, Err: os.ErrNotExist}
		if rp.External {
			return "", nil, fmt.Errorf("read %s: %s", displayPath, rp.ErrorText(err))
		}
		return "", nil, err
	}
	// Directories and the host overlay (unsaved editor buffers) are never
	// images — let Execute produce its canonical messages for those.
	if info, err := os.Stat(p.Path); err == nil && info.IsDir() {
		return "", nil, fmt.Errorf("%s is a directory, not a file — use the ls tool to list it, or read a specific file inside it", displayPath)
	}
	f, err := os.Open(p.Path)
	if err != nil {
		if rp.External {
			return "", nil, fmt.Errorf("read %s: %s", displayPath, rp.ErrorText(err))
		}
		return "", nil, fmt.Errorf("read %s: %w", displayPath, err)
	}
	defer f.Close()

	// Peek enough to sniff the format; raster images are handled here, anything
	// else goes through the text pipeline below.
	peek := make([]byte, readFileBinaryPeek)
	pn, _ := io.ReadFull(f, peek)
	peek = peek[:pn]
	if mime := http.DetectContentType(peek); isRasterMime(mime) {
		return r.readImage(ctx, displayPath, f, peek, mime)
	}
	text, err := r.Execute(ctx, args)
	return text, nil, err
}

// readImage handles a raster image read: clamps/re-encodes via
// imageopt.CompressForRead, appends a vision-proxy transcript (VLM/OCR) for
// text-only models, and returns the compressed payload as a data URL.
// Mirrors jcode's read-time clamp+vision so multica cards behave identically
// across runtimes.
func (r readFile) readImage(ctx context.Context, displayPath string, f *os.File, peek []byte, mime string) (string, []string, error) {
	rest, err := io.ReadAll(f)
	if err != nil {
		return "", nil, fmt.Errorf("read %s: %w", displayPath, err)
	}
	raw := append(peek, rest...)
	data, outMime, w, h := imageopt.CompressForRead(raw, mime, imageQuality())
	img := "data:" + outMime + ";base64," + base64.StdEncoding.EncodeToString(data)

	var b strings.Builder
	fmt.Fprintf(&b, "[image: %s] %s (%d×%d, %.1f KB", outMime, displayPath, w, h, float64(len(data))/1024)
	if len(data) != len(raw) || outMime != mime {
		fmt.Fprintf(&b, ", compressed from %.1f KB %s", float64(len(raw))/1024, mime)
	}
	b.WriteString(")\n")
	if text := visionProbe(ctx, data); text != "" {
		b.WriteString(text)
	}
	return truncateImageText(b.String()), []string{img}, nil
}

// visionProbe sends the (already clamped/re-encoded) image to the local
// vision-proxy and returns a model-facing transcript. Best-effort: returns ""
// when disabled or on any error (the read still succeeds).
func visionProbe(ctx context.Context, data []byte) string {
	url := strings.TrimSpace(os.Getenv(reasonixVisionURLEnv))
	if url == "" {
		return ""
	}
	task := strings.TrimSpace(os.Getenv(reasonixVisionTaskEnv))
	if task == "" {
		task = "document"
	}
	timeout := 120
	if v := os.Getenv(reasonixVisionTimeoutEnv); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			timeout = n
		}
	}
	body, _ := json.Marshal(map[string]string{
		"task":  task,
		"image": base64.StdEncoding.EncodeToString(data),
	})
	reqCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return ""
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var out struct {
		Text  string `json:"text"`
		Model string `json:"model"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return ""
	}
	if out.Text == "" {
		return ""
	}
	return fmt.Sprintf("\n[vision: %s]\n%s\n", orDefault(out.Model, task), strings.TrimSpace(out.Text))
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func imageQuality() int {
	if q := os.Getenv(reasonixImageQualityEnv); q != "" {
		if n, err := strconv.Atoi(q); err == nil {
			return n
		}
	}
	return 80
}

func isRasterMime(mime string) bool {
	switch mime {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	}
	return false
}

func truncateImageText(s string) string {
	if len(s) <= readImageMaxOutputBytes {
		return s
	}
	head := s[:readImageMaxOutputBytes/2]
	tail := s[len(s)-readImageMaxOutputBytes/4:]
	return head + fmt.Sprintf("\n...[OCR text truncated: %d bytes total]...\n", len(s)) + tail
}

// scan reads lines from src and returns the formatted output with line numbers.
func (r readFile) scan(src io.Reader, offset, limit int) (string, error) {
	scanner := bufio.NewScanner(src)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var collected []string
	lineNo := 0
	hasMore := false
	for scanner.Scan() {
		lineNo++
		if lineNo <= offset {
			continue
		}
		if len(collected) < limit {
			collected = append(collected, scanner.Text())
			continue
		}
		// A line past the requested window exists — stop here rather than reading
		// the rest of the file just to count the remainder.
		hasMore = true
		break
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("scan: %w", err)
	}

	if lineNo == 0 {
		return "(empty file)", nil
	}
	if len(collected) == 0 {
		return fmt.Sprintf("(offset %d is past EOF — file has %d lines)", offset, lineNo), nil
	}

	maxShown := offset + len(collected)
	w := len(fmt.Sprint(maxShown))

	var b strings.Builder
	for i, line := range collected {
		fmt.Fprintf(&b, "%*d→%s\n", w, offset+i+1, line)
	}
	if hasMore {
		fmt.Fprintf(&b, "\n[more lines below; pass offset=%d to continue]\n", offset+len(collected))
	}
	return b.String(), nil
}
