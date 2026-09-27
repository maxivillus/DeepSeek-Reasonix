// Package builtin provides Reasonix's compile-time built-in tools. Each tool
// self-registers via init(); main blank-imports this package to wire them in.
package builtin

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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
	readFileBinaryPeek        = 8 * 1024   // bytes scanned for NUL before reading further
	readFileDetectSample      = 256 * 1024 // bytes sampled for encoding detection before streaming
	readFileMaxLineBytes      = 1024 * 1024
	readFileMaxFormattedBytes = 8 << 20
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
	overlay  FileOverlay
	captured *tool.ReadResultSource
}

const (
	readFileDefaultLimit = 2000 // lines returned when limit is unset
)

// readFileParams is one validated read_file call with defaults applied.
type readFileParams struct {
	Path        string
	Intent      tool.ReadIntent
	WindowGiven bool
	Offset      int
	Limit       int
}

const (
	readFileEmptyOutput = "(empty file)"
	readFilePastEOFTail = " is past EOF — file has "
)

// readWindowGiven reports whether the call named an explicit line window.
func readWindowGiven(args json.RawMessage) bool {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(args, &fields); err != nil {
		return false
	}
	_, offset := fields["offset"]
	_, limit := fields["limit"]
	return offset || limit
}

// parseReadFileParams validates one read_file call and applies the documented
// defaults, so Execute and ReadEnvelope agree on what was requested.
func parseReadFileParams(args json.RawMessage) (readFileParams, error) {
	var p struct {
		Path   string `json:"path"`
		Intent string `json:"intent,omitempty"`
		Offset int    `json:"offset,omitempty"`
		Limit  int    `json:"limit,omitempty"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return readFileParams{}, fmt.Errorf("invalid args: %w", err)
	}
	if p.Path == "" {
		return readFileParams{}, fmt.Errorf("path is required")
	}
	windowGiven := readWindowGiven(args)
	intent, err := readIntentFor(p.Intent, windowGiven)
	if err != nil {
		return readFileParams{}, err
	}
	if p.Offset < 0 {
		p.Offset = 0
	}
	if p.Limit <= 0 {
		p.Limit = readFileDefaultLimit
	}
	return readFileParams{Path: p.Path, Intent: intent, WindowGiven: windowGiven, Offset: p.Offset, Limit: p.Limit}, nil
}

// readIntentFor resolves the effective read intent and rejects combinations
// that would leave the caller unsure which promise the call made.
func readIntentFor(explicit string, windowGiven bool) (tool.ReadIntent, error) {
	switch tool.ReadIntent(strings.TrimSpace(explicit)) {
	case "":
		if windowGiven {
			return tool.ReadIntentRange, nil
		}
		return tool.ReadIntentInspect, nil
	case tool.ReadIntentInspect:
		return tool.ReadIntentInspect, nil
	case tool.ReadIntentRange:
		if !windowGiven {
			return "", fmt.Errorf("intent=range requires an explicit offset or limit; pass the window to read, or use intent=inspect for a bounded preview")
		}
		return tool.ReadIntentRange, nil
	case tool.ReadIntentFull:
		// Kept as a compatibility hint. Every call remains one bounded window;
		// the host no longer creates a whole-file debt or completion gate.
		return tool.ReadIntentFull, nil
	default:
		return "", fmt.Errorf("intent must be inspect, range, or full (got %q)", explicit)
	}
}

func (readFile) Name() string { return "read_file" }

func (readFile) Description() string {
	return "Read one bounded text window with optional line offset/limit. Output prefixes each line with its 1-based number. Any successful window observes the current file version for later structured edits. Use the next-window hint to page only when more content is useful. Legacy intent and cursor fields are accepted as navigation hints and never create a whole-file completion requirement. Raster images (PNG/JPEG/GIF/WebP) are handled specially: the image is clamped to 1568px and re-encoded as JPEG (quality 80)."
}

func (readFile) Schema() json.RawMessage {
	return json.RawMessage(`{
"type":"object",
"properties":{
  "path":{"type":"string","description":"File path"},
	"intent":{"type":"string","enum":["inspect","range","full"],"description":"Compatibility hint. Every value reads only this bounded window and creates no whole-file obligation."},
	"cursor":{"type":"string","description":"Optional continuation cursor from a prior result. Invalid legacy cursors should be replaced with an explicit offset and limit."},
  "offset":{"type":"integer","description":"0-based line offset to start reading from (default 0)","minimum":0},
  "limit":{"type":"integer","description":"Maximum lines to return (default 2000)","minimum":1}
},
"required":["path"]
}`)
}

func (readFile) ReadOnly() bool { return true }

// ReadEnvelope reports what one read_file call delivered. The source identity
// comes from the store that actually served the content, the snapshot stays
// constant across the pages of one logical read, and the window digest covers
// only this page's delivered lines. read_id, result_ref and workspace_id are
// host identity the agent fills in.
func (r readFile) ReadEnvelope(ctx context.Context, args json.RawMessage, output string) (tool.ReadResultEnvelope, bool) {
	p, err := parseReadFileParams(args)
	if err != nil {
		return tool.ReadResultEnvelope{}, false
	}
	rp := resolveReadablePath(r.workDir, p.Path, r.paths)
	env := tool.ReadResultEnvelope{
		ProtocolVersion: tool.ReadResultProtocolVersion,
		Source:          tool.ReadResultSource{CanonicalPath: rp.Path},
		Intent:          p.Intent,
	}
	if p.WindowGiven {
		requested := tool.ReadRange{Start: p.Offset, End: p.Offset + p.Limit}
		env.RequestedRange = &requested
	}

	// The store that served the content owns the identity: an unsaved editor
	// buffer must never be proven by the disk file's identity.
	if r.captured != nil {
		env.Source = *r.captured
	}
	env.Source.Snapshot = tool.SourceSnapshot(env.Source.Kind, rp.Path, env.Source.Identity)

	window, hasWindow := tool.ParseReadWindow(output)
	if hasWindow {
		env.DeliveredRanges = []tool.ReadRange{window.Range()}
		env.WindowDigest = tool.WindowDigest(rp.Path, window)
	}
	trailer := tool.ParseReadTrailer(output)
	env.HasMore = trailer.HasMore
	env.EOF = !trailer.HasMore
	switch {
	case trailer.LocalSafety:
		env.SourceCut = tool.ReadCutSafetyPage
	case trailer.HasMore:
		env.SourceCut = tool.ReadCutPageLimit
	}
	if env.EOF {
		if end, ok := readSourceEnd(output, window, hasWindow); ok {
			env.SourceEnd = &end
		}
	}
	if trailer.HasMore {
		env.NextCursor = tool.EncodeReadCursor(tool.ReadCursor{
			Path:      rp.Path,
			Snapshot:  env.Source.Snapshot,
			NextStart: trailer.NextOffset,
		})
	}
	return env, true
}

// overlayText mirrors Execute's overlay routing so the envelope names the same
// store that produced the delivered bytes.
func (r readFile) overlayText(ctx context.Context, rp ResolvedPath) (string, bool) {
	if r.overlay == nil || rp.External || !filepath.IsAbs(rp.Path) {
		return "", false
	}
	return r.overlay.ReadTextFile(ctx, rp.Path)
}

func digestText(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// readSourceEnd recovers the source's zero-based end line index from the
// reader's own result text: a complete window ends at its last line, and the
// empty-file / past-EOF markers state the count directly.
func readSourceEnd(output string, window tool.ReadWindow, hasWindow bool) (int, bool) {
	if hasWindow {
		return window.Range().End, true
	}
	trimmed := strings.TrimSpace(output)
	if trimmed == readFileEmptyOutput {
		return 0, true
	}
	if rest, ok := strings.CutPrefix(trimmed, "(offset "); ok {
		if _, tail, found := strings.Cut(rest, readFilePastEOFTail); found {
			if n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimSpace(tail), " lines)")); err == nil && n >= 0 {
				return n, true
			}
		}
	}
	return 0, false
}

// SnipHint front-loads file content: the most relevant lines are near the top,
// so keep a generous head and a short tail when an old read is shortened.
func (readFile) SnipHint() tool.SnipHint {
	return tool.SnipHint{Head: 120, Tail: 12, HeadChars: 12000, TailChars: 2000}
}

func (r readFile) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	output, _, err := r.ExecuteRead(ctx, args)
	return output, err
}

func (r readFile) scanEncoded(f io.Reader, offset, limit int) (string, error) {

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
		enc := fileenc.DetectQuick(peek)
		return r.scan(transform.NewReader(io.MultiReader(bytes.NewReader(peek), f), fileenc.Decoder(enc)), offset, limit)
	case fileenc.UTF8BOM:
		// Strip the 3-byte BOM; the content is valid UTF-8 and streams directly.
		body := peek
		if len(body) >= 3 {
			body = body[3:]
		}
		return r.scan(io.MultiReader(bytes.NewReader(body), f), offset, limit)
	}

	// BOM-less UTF-16 (Windows source files) has a NUL for every ASCII char but
	// no BOM, so it reaches here; recognise it by its NUL pattern and decode it
	// rather than rejecting it as binary.
	if k, ok := fileenc.DetectUTF16NoBOM(peek); ok {
		return r.scan(transform.NewReader(io.MultiReader(bytes.NewReader(peek), f), fileenc.Decoder(k)), offset, limit)
	}

	if bytes.IndexByte(peek, 0) >= 0 {
		return "", fmt.Errorf("binary file (NUL byte detected); use a binary inspection tool")
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

	// When more file follows, the sample can end inside a character.
	detect := fileenc.Detect
	if !peekEOF {
		detect = fileenc.DetectFragment
	}
	enc, _ := detect(head)

	src := io.MultiReader(bytes.NewReader(head), f)
	if dec := fileenc.Decoder(enc); dec != nil {
		return r.scan(transform.NewReader(src, dec), offset, limit)
	}
	return r.scan(src, offset, limit)
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
	scanner.Buffer(make([]byte, 0, 64*1024), readFileMaxLineBytes)

	var collected []string
	textBytes := 0
	lineNo := 0
	hasMore := false
	safetyPaged := false
	requestedEnd := offset + limit
	for scanner.Scan() {
		lineNo++
		if lineNo <= offset {
			continue
		}
		if len(collected) < limit {
			line := scanner.Text()
			count := len(collected) + 1
			width := len(strconv.Itoa(offset + count))
			nextOffset := offset + count
			bodyBytes := textBytes + len(line) + count*(width+len("→")+1)
			trailer := readFileSafetyTrailer(nextOffset, requestedEnd)
			if bodyBytes+len(trailer) > readFileMaxFormattedBytes {
				hasMore = true
				safetyPaged = true
				break
			}
			collected = append(collected, line)
			textBytes += len(line)
			continue
		}
		// A line past the requested window exists — stop here rather than reading
		// the rest of the file just to count the remainder.
		hasMore = true
		break
	}
	if err := scanner.Err(); err != nil {
		if strings.Contains(err.Error(), "token too long") {
			return "", fmt.Errorf("scan: source line exceeds the 1 MiB local safety limit: %w", err)
		}
		return "", fmt.Errorf("scan: %w", err)
	}

	if lineNo == 0 {
		return readFileEmptyOutput, nil
	}
	if len(collected) == 0 {
		return fmt.Sprintf("(offset %d%s%d lines)", offset, readFilePastEOFTail, lineNo), nil
	}

	maxShown := offset + len(collected)
	w := len(fmt.Sprint(maxShown))

	var b strings.Builder
	for i, line := range collected {
		fmt.Fprintf(&b, "%*d→%s\n", w, offset+i+1, line)
	}
	if safetyPaged {
		b.WriteString(readFileSafetyTrailer(offset+len(collected), requestedEnd))
	} else if hasMore {
		fmt.Fprintf(&b, "\n[PARTIAL view: showing lines %d-%d of at least %d; pass offset=%d to continue. A partial window may be sufficient for local work.]\n", offset+1, maxShown, lineNo, maxShown)
	}
	return b.String(), nil
}

func readFileSafetyTrailer(nextOffset, requestedEnd int) string {
	return fmt.Sprintf("\n[read_file local safety page; next_offset=%d requested_end=%d]\n", nextOffset, requestedEnd)
}
