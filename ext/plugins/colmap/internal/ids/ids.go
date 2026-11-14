// Package ids makes object ids: an OpenAI-style prefix and 24 random hex digits.
package ids

import (
	"crypto/rand"
	"encoding/hex"
)

// New returns prefix + 24 hex digits ("file-…", "upload_…", "recon_…").
func New(prefix string) string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}
