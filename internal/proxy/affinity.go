package proxy

import (
	"crypto/sha256"
	"encoding/hex"
)

type affinityCtxKey struct{}

// sessionAffinityValue is the per-conversation value sent in a provider's
// session_affinity_header. conv is the slot cache's conversation key, which is
// already a hash that includes the caller key; hashing it again under a fixed
// prefix keeps the value stable per conversation, one-way, and unrelated to the
// id the activity log and slot cache show. The caller's key never enters it.
func sessionAffinityValue(conv string) string {
	h := sha256.Sum256([]byte("corrallm-session-affinity\x00" + conv))
	return hex.EncodeToString(h[:])[:32]
}
