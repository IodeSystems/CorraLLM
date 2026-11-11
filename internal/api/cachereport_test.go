package api

import (
	"context"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/proc"
	"github.com/iodesystems/corrallm/internal/proxy"
	"github.com/iodesystems/corrallm/internal/sched"
	"github.com/iodesystems/corrallm/internal/store"
)

// cacheReport reads the window's rows and the model's flags from its cmd.
func TestCacheReportReadsRowsAndFlags(t *testing.T) {
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := &config.Config{Models: map[string]config.Model{
		"swift": {Cmd: "llama-server --cache-ram 18432 --ctx-checkpoints 8 --parallel 2"}}}
	mgr := proc.NewManager(cfg)
	t.Cleanup(mgr.Shutdown)
	h := &Handlers{Store: st, Proxy: proxy.New(cfg, mgr, sched.New(), st)}

	now := time.Now().UnixMilli()
	for i, class := range []string{"cold", "hit", "reload"} {
		if _, err := st.InsertActivityID(store.Activity{TS: now - int64(3-i)*1000, Served: "swift", Path: "/v1/chat/completions",
			Status: 200, PromptTokens: 9000, Conversation: "c", ConversationMessages: 1,
			Cache: &store.CacheRow{Served: "swift", Conversation: "c", PromptTokens: 9000, LostTok: 5000 * i, Class: class, Cause: "crash"}}); err != nil {
			t.Fatal(err)
		}
	}
	out, err := h.CacheReport(context.Background(), &CacheReportInput{Served: "swift", Minutes: 60})
	if err != nil {
		t.Fatal(err)
	}
	r := out.Body
	if r.Totals.Requests != 3 || r.Totals.Misses != 1 || r.Totals.LostTokens != 10000 {
		t.Errorf("totals = %+v", r.Totals)
	}
	if r.Settings.CacheRAMMiB != 18432 || r.Settings.CtxCheckpoints != 8 || r.Settings.Parallel != 2 {
		t.Errorf("settings = %+v", r.Settings)
	}
	if len(r.ReloadReasons) != 1 || r.ReloadReasons[0].Reason != "crash" {
		t.Errorf("reload reasons = %+v", r.ReloadReasons)
	}
	if _, err := h.CacheReport(context.Background(), &CacheReportInput{Served: "nope", Minutes: 60}); err == nil {
		t.Error("an unknown model should be refused")
	}
}
