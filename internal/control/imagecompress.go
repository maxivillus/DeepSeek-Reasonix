package control

import (
	"reasonix/internal/imageopt"
)

// maxVisionDim aliases the shared vision budget (imageopt) so existing control
// tests keep compiling; new code should use imageopt directly.
const maxVisionDim = imageopt.MaxVisionDim

// compressForVision downscales an oversized image to the shared vision budget
// and re-encodes it — PNG/GIF stay lossless (screenshots, text, transparency),
// JPEG/WebP go to JPEG. Best-effort: an undecodable format, a decode/encode
// failure, or an image already within budget returns the original bytes and
// mime unchanged. Shared implementation lives in imageopt so the read_file
// builtin reuses the same budget.
func compressForVision(raw []byte, mime string) ([]byte, string) {
	return imageopt.CompressForVision(raw, mime)
}
