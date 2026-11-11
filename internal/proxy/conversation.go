package proxy

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/iodesystems/corrallm/internal/cacheattr"
	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/proc"
	"github.com/iodesystems/corrallm/internal/slotcache"
	"github.com/iodesystems/corrallm/internal/store"
)

// conversation is which conversation a request belongs to and how many
// messages it carried, stamped onto its activity row by logReq, and the
// turn's block hashes for the prompt-cache tracker.
type conversation struct {
	id       string
	messages int
	turn     *slotcache.Turn
}

type conversationCtxKey struct{}

// SetCacheTracker attaches the prompt-cache tracker (P75); nil turns
// attribution off.
func (p *Proxy) SetCacheTracker(t *cacheattr.Tracker) { p.cacheTracker = t }

// CacheTracker is the attached tracker, nil when off.
func (p *Proxy) CacheTracker() *cacheattr.Tracker { return p.cacheTracker }

// attributeCache classifies a served chat turn's prompt-cache use, for the
// activity row. Local backends only (a cmd, and a process with a handle): a
// remote provider's cache is not ours to explain. nil whenever there is
// nothing to say, and on any panic — attribution never fails a request.
func (p *Proxy) attributeCache(r *http.Request, pr *proc.Process, served string, backend config.Model, u usage) (row *store.CacheRow) {
	if p.cacheTracker == nil || backend.Cmd == "" || pr == nil || r.URL.Path != "/v1/chat/completions" {
		return nil
	}
	c, ok := r.Context().Value(conversationCtxKey{}).(conversation)
	if !ok || c.turn == nil {
		return nil
	}
	defer func() {
		if e := recover(); e != nil {
			slog.Warn("prompt-cache attribution skipped", "served", served, "panic", e)
			row = nil
		}
	}()
	inst := pr.Instance()
	if inst == "" {
		return nil
	}
	slot := -1
	if u.IDSlot != nil {
		slot = *u.IDSlot
	}
	got, ok := p.cacheTracker.Observe(cacheattr.Obs{
		Served: served, Instance: inst, Slot: slot, TS: time.Now().UnixMilli(),
		PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens, CachedTokens: u.CachedTokens,
		PromptPerSec: u.PromptPerSec, CtxCheckpoints: strings.Contains(backend.Cmd, "--ctx-checkpoints"),
		Turn: c.turn,
	})
	if !ok {
		return nil
	}
	return &got
}
