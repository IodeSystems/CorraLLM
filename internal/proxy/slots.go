package proxy

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/slotcache"
)

// minSlotBytes is the floor below which swapping costs more than it saves.
//
// A save is ~0.64 s regardless of how small the conversation is, and a short
// prompt is cheap to reprocess — at the measured ~1,800 prompt tokens/s a 4,000
// token prompt is about 1.7 s, so below roughly that the machinery is a tax on
// the request rather than a saving. 16 KB of request body is ~4,000 tokens at
// the usual 4 bytes per token; bytes are used rather than a token count because
// corrallm has not tokenised anything at this point and a count would cost more
// than the decision is worth.
//
// Deliberately blunt. The number that matters is not this threshold's precision;
// it is that a two-line request never pays 0.64 s to be remembered.
const minSlotBytes = 16 << 10

// swapSlot makes this request's conversation the one resident in the backend's
// slot, saving whichever conversation was there.
//
// Off unless a slot cache directory was configured, and a no-op for anything it
// cannot help: a remote backend (somebody else's slot, and no endpoint to ask),
// a backend that answered "cannot" once already, a body it cannot key, and a
// prompt small enough that reprocessing is cheaper than remembering.
//
// It never fails the request. The worst case is the behaviour of every request
// before this existed.
func (p *Proxy) swapSlot(ctx context.Context, target *config.ProxyTarget, served string, backend config.Model, callerKey string, r *http.Request, body []byte) {
	if p.slots == nil || target == nil || target.URL == nil {
		return
	}
	// Only a backend corrallm STARTED has a slot corrallm may touch. A model with
	// no cmd is somebody else's endpoint — a remote provider, or a process that
	// merely happens to be listening — and asking that for /slots would be asking
	// a stranger about their cache.
	//
	// The cmd is the WHOLE test, and it is worth saying why nothing else is. The
	// obvious second check — "Target.Model is empty, so this is not a remote" —
	// is wrong: ProxyTarget() copies a model's `upstream:` into Target.Model, and
	// for a LOCAL model upstream is the HuggingFace repo it is downloaded from
	// (`unsloth/Qwen3.8-27B-GGUF:UD-Q6_K`), not a provider's model id. That check
	// shipped, and it silently skipped every request on the one backend this
	// feature exists for: the cache was on, the flags were right, and nothing
	// ever happened.
	if backend.Cmd == "" {
		return
	}
	if len(body) < minSlotBytes {
		return
	}
	// Only the JSON chat/completions shape carries a conversation. Audio is
	// multipart and has no prefix worth keeping.
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		return
	}

	key, ok := slotcache.Key(callerKey, r.Header.Get(slotcache.ConversationHeader), body)
	if !ok {
		return
	}
	// The namespace is what the backend IS: a state saved by one command at one
	// context size is meaningless to another, and must not be found by it.
	ns := slotcache.Namespace(served, backend.Cmd, backend.MaxTokens)
	client := &slotcache.Client{BaseURL: strings.TrimRight(target.URL.String(), "/")}

	what, restored := p.slots.Swap(ctx, client, served, ns, key)
	if what == "" && !restored {
		return // already resident, or nothing to do
	}
	// One line per actual swap, at info: this is a thing that happened to a
	// caller's latency, and a silent 1-second pause is the kind of mystery this
	// whole exercise exists to stop creating.
	slog.Info("slot cache", "backend", served, "caller", callerKey, "restored", restored, "detail", what)
}

// SetSlotCache turns the slot cache on, pointed at a directory.
//
// OFF unless an operator names a directory, because it trades disk for latency
// at a scale only they can judge: 36.8 KB per token means a 50k-token session is
// 1.9 GB, and ten of them is a fifth of the default 20 GB cap.
func (p *Proxy) SetSlotCache(dir string, maxBytes int64) {
	if strings.TrimSpace(dir) == "" {
		p.slots = nil
		return
	}
	p.slots = &slotcache.Manager{
		Store: &slotcache.Store{Dir: dir, MaxBytes: maxBytes},
	}
	slog.Info("slot cache on", "dir", dir, "maxBytes", maxBytes)
}
