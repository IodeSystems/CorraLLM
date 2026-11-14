package slotcache

import (
	"strings"
	"testing"
)

// withPCK adds a top-level prompt_cache_key to a chat body.
func withPCK(body []byte, key string) []byte {
	return []byte(`{"prompt_cache_key":"` + key + `",` + strings.TrimPrefix(string(body), "{"))
}

// prompt_cache_key is the conversation: two turns whose heads differ (a
// client pruned the head) keep one key, Key and Inspect agree, and the body's
// key equals a declared header of the same string.
func TestPromptCacheKeyIsTheConversation(t *testing.T) {
	t1 := withPCK(chat([2]string{"system", "v1"}, [2]string{"user", "start"}), "sess-1")
	t2 := withPCK(chat([2]string{"system", "v2 pruned"}, [2]string{"user", "later"}), "sess-1")
	k1, ok1 := Key("sk-a", "", t1)
	k2, ok2 := Key("sk-a", "", t2)
	if !ok1 || !ok2 || k1 != k2 {
		t.Fatalf("same prompt_cache_key, keys %q %q (ok %v %v): want one key", k1, k2, ok1, ok2)
	}
	in, ok := Inspect("sk-a", "", t2)
	if !ok || in.Key != k1 || in.Messages != 2 {
		t.Errorf("Inspect key %q messages %d, want %q 2", in.Key, in.Messages, k1)
	}
	if h, _ := Key("sk-a", "sess-1", nil); h != k1 {
		t.Errorf("header key %q != body key %q for the same string", h, k1)
	}
}

// Two callers that send the same string are two conversations.
func TestPromptCacheKeyScopedByCaller(t *testing.T) {
	b := withPCK(chat([2]string{"user", "hi"}), "shared")
	a, _ := Key("sk-a", "", b)
	c, _ := Key("sk-b", "", b)
	ia, _ := Inspect("sk-a", "", b)
	ic, _ := Inspect("sk-b", "", b)
	if a == c || ia.Key == ic.Key {
		t.Error("two callers with one prompt_cache_key share a conversation key")
	}
}

// Absent or blank: the inferred key, exactly as before.
func TestPromptCacheKeyAbsentUnchanged(t *testing.T) {
	plain := chat([2]string{"system", "s"}, [2]string{"user", "u"})
	inferred, _ := Key("sk-a", "", plain)
	if k, _ := Key("sk-a", "", withPCK(plain, "  ")); k != inferred {
		t.Error("a blank prompt_cache_key changed the inferred key")
	}
	if in, _ := Inspect("sk-a", "", plain); in.Key != inferred {
		t.Error("Inspect without prompt_cache_key differs from the inferred key")
	}
	// The header alone still names the conversation.
	hdr, _ := Key("sk-a", "from-header", nil)
	if k, _ := Key("sk-a", "from-header", plain); k != hdr {
		t.Error("without prompt_cache_key the X-Corrallm-Conversation header must name the conversation")
	}
}

// prompt_cache_key wins over the X-Corrallm-Conversation header.
func TestPromptCacheKeyBeatsHeader(t *testing.T) {
	plain := chat([2]string{"system", "s"}, [2]string{"user", "u"})
	body, _ := Key("sk-a", "", withPCK(plain, "from-body"))
	if k, _ := Key("sk-a", "from-header", withPCK(plain, "from-body")); k != body {
		t.Error("Key: prompt_cache_key must win over the header")
	}
	if in, _ := Inspect("sk-a", "from-header", withPCK(plain, "from-body")); in.Key != body {
		t.Error("Inspect: prompt_cache_key must win over the header")
	}
}
