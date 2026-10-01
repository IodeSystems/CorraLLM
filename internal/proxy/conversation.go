package proxy

import "github.com/iodesystems/corrallm/internal/slotcache"

// conversation is which conversation a request belongs to and how many
// messages it carried, stamped onto its activity row by logReq.
type conversation struct {
	id       string
	messages int
}

type conversationCtxKey struct{}

// conversationOf names the conversation a chat request belongs to — the slot
// cache's key, so the activity log and the KV cache agree on what one
// conversation is — and counts its messages. ok is false for a body that
// carries no conversation.
func conversationOf(caller, declared string, body []byte) (id string, messages int, ok bool) {
	id, ok = slotcache.Key(caller, declared, body)
	if !ok {
		return "", 0, false
	}
	return id, slotcache.Messages(body), true
}
