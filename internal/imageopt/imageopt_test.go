package imageopt

// The decompression-bomb guard must not be defeated by integer width: `int` is
// 32 bits on 32-bit targets, so a plain width*height product overflows to a
// negative value and the guard would pass a bomb through.

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"testing"
)

func TestDecodeBombGuard(t *testing.T) {
	cases := []struct {
		name string
		w, h int
		want bool
	}{
		{"exactly at budget", 50000, 1000, false},
		{"one pixel over budget", 50001, 1000, true},
		{"product overflows int32", 46341, 46341, true},
		{"large power of two", 65536, 65536, true},
		{"degenerate", 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := decodeBomb(tc.w, tc.h); got != tc.want {
				t.Fatalf("decodeBomb(%d, %d) = %v, want %v", tc.w, tc.h, got, tc.want)
			}
		})
	}
}

// bombPNG writes a PNG whose IHDR declares absurd dimensions while the payload
// stays tiny — the shape a decompression bomb takes on the wire.
func bombPNG(t *testing.T, w, h uint32) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.Write([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a})
	chunk := func(typ string, data []byte) {
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len(data)))
		buf.Write(length[:])
		buf.WriteString(typ)
		buf.Write(data)
		sum := crc32.NewIEEE()
		sum.Write([]byte(typ))
		sum.Write(data)
		var crc [4]byte
		binary.BigEndian.PutUint32(crc[:], sum.Sum32())
		buf.Write(crc[:])
	}
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], w)
	binary.BigEndian.PutUint32(ihdr[4:], h)
	ihdr[8] = 8 // bit depth
	ihdr[9] = 2 // truecolour
	chunk("IHDR", ihdr)
	chunk("IEND", nil)
	return buf.Bytes()
}

func TestCompressForReadRejectsBombHeader(t *testing.T) {
	raw := bombPNG(t, 65536, 65536)
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(raw)); err == nil && !decodeBomb(cfg.Width, cfg.Height) {
		t.Fatalf("fixture header is not actually a bomb: %dx%d", cfg.Width, cfg.Height)
	}
	out, mime, w, h := CompressForRead(raw, "image/png", 0)
	if !bytes.Equal(out, raw) || mime != "image/png" || w != 0 || h != 0 {
		t.Fatalf("bomb header was not returned untouched: len=%d mime=%s %dx%d", len(out), mime, w, h)
	}
}
