package memory

import "strings"

// TrustLevel grades how strongly a fact may be relied on without re-
// verification — the storage-side proxy for "irrefutable". The empty value
// (and the default for facts saved before this field existed) is Medium.
type TrustLevel string

const (
	TrustHigh   TrustLevel = "high"   // explicitly confirmed by the user
	TrustMedium TrustLevel = "medium" // default; auto-extracted from sessions
	TrustLow    TrustLevel = "low"    // unverified / hand-marked as unreliable
)

// NormalizeTrust maps any input to a valid TrustLevel; empty → Medium.
func NormalizeTrust(s string) TrustLevel {
	switch TrustLevel(strings.ToLower(strings.TrimSpace(s))) {
	case TrustHigh:
		return TrustHigh
	case TrustLow:
		return TrustLow
	default:
		return TrustMedium
	}
}

// TrustMultiplier is the recall score weight (jcode-inspired): high-trust
// facts rank above equal medium-trust facts, low-trust facts sink.
func TrustMultiplier(t TrustLevel) float64 {
	switch NormalizeTrust(string(t)) {
	case TrustHigh:
		return 1.5
	case TrustLow:
		return 0.7
	default:
		return 1.0
	}
}
