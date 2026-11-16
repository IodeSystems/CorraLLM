package proxy

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/iodesystems/corrallm/internal/cacheattr"
	"github.com/iodesystems/corrallm/internal/config"
)

// fakeLlamaProps serves Swift's /props and /metrics fixtures.
func fakeLlamaProps(t *testing.T) *httptest.Server {
	t.Helper()
	props, err := os.ReadFile("../cacheattr/testdata/props.json")
	if err != nil {
		t.Fatal(err)
	}
	metrics, err := os.ReadFile("../cacheattr/testdata/metrics_patched.txt")
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/props":
			_, _ = w.Write(props)
		case "/metrics":
			_, _ = w.Write(metrics)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

const swiftTestCmd = "llama-server --parallel 3 --kv-unified -c 180224 --cache-ram 27648 --ctx-checkpoints 8"

// swiftTestGeo is Swift's startup-log geometry (see cacheattr's tests).
var swiftTestGeo = cacheattr.Geometry{
	NCtxSlot: 86016, KVCellBytes: 6688.0 * (1 << 20) / 180224, RSSeqBytes: 2244.38 * (1 << 20) / 15,
	CheckpointBytes: 149.646 * (1 << 20), CacheRAMBytes: 27648 << 20,
}

// detectRig is a proxy whose "swift" model's backend reported total_slots 3.
func detectRig(t *testing.T, maxConcurrent int) (http.Handler, *Proxy, config.Model) {
	t.Helper()
	be := fakeLlamaProps(t)
	m := modelTo(t, be.URL, "chat")
	m.Cmd, m.MaxConcurrent = swiftTestCmd, maxConcurrent
	h, p := capRig(t, capStore(t), map[string]config.Model{"swift": m})
	poller := cacheattr.NewPoller(func() []cacheattr.Target {
		return []cacheattr.Target{{Served: "swift", Instance: "i", BaseURL: be.URL, Cmd: swiftTestCmd, Geometry: swiftTestGeo}}
	}, 0)
	poller.PollOnce(context.Background())
	p.SetCachePoller(poller)
	return h, p, m
}

// maxConcurrent unset: admit the backend's total_slots, not the default 1.
// Before any backend reports, the default stands.
func TestAdmitSlotsAutoWhenUnset(t *testing.T) {
	_, p, m := detectRig(t, 0)
	if got := p.AdmitSlots("swift", m); got != 3 {
		t.Errorf("auto slots = %d, want total_slots 3", got)
	}
	p.SetCachePoller(cacheattr.NewPoller(func() []cacheattr.Target { return nil }, 0))
	if got := p.AdmitSlots("swift", m); got != 1 {
		t.Errorf("nothing reported = %d, want default 1", got)
	}
	m.Cmd = "" // not a local backend: never detected
	if _, ok := p.DetectedCapacity("swift", m); ok {
		t.Error("a model without a cmd was detected")
	}
}

// maxConcurrent set and different: config wins, one WARN per change.
func TestAdmitSlotsConfigWinsAndWarnsOnce(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	_, p, m := detectRig(t, 2)
	for range 3 {
		if got := p.AdmitSlots("swift", m); got != 2 {
			t.Fatalf("slots = %d, want config 2", got)
		}
	}
	if n := strings.Count(buf.String(), "maxConcurrent 2 but backend reports total_slots 3"); n != 1 {
		t.Fatalf("warned %d times, want once:\n%s", n, buf.String())
	}
	m.MaxConcurrent = 4
	p.AdmitSlots("swift", m)
	if !strings.Contains(buf.String(), "maxConcurrent 4 but backend reports total_slots 3") {
		t.Error("a changed config did not warn again")
	}
	buf.Reset()
	m.MaxConcurrent = 3
	p.AdmitSlots("swift", m)
	if buf.Len() != 0 {
		t.Errorf("a matching config warned: %s", buf.String())
	}
}

// /v1/models and /v1/models/{id} carry the detected counts: slots 3 (auto),
// cache_slots 9 (guaranteed), cache_slots_typical 15 (hour mean).
func TestModelsCacheSlots(t *testing.T) {
	h, _, _ := detectRig(t, 0)
	type entry struct {
		ID                string `json:"id"`
		Slots             int    `json:"slots"`
		CacheSlots        int    `json:"cache_slots"`
		CacheSlotsTypical int    `json:"cache_slots_typical"`
	}
	var out struct {
		Data []entry `json:"data"`
	}
	if c := getJSON(t, h, "/v1/models", &out); c != 200 {
		t.Fatalf("status %d", c)
	}
	want := entry{ID: "swift", Slots: 3, CacheSlots: 9, CacheSlotsTypical: 15}
	if len(out.Data) != 1 || out.Data[0] != want {
		t.Errorf("got %+v, want %+v", out.Data, want)
	}
	var one entry
	if c := getJSON(t, h, "/v1/models/swift", &one); c != 200 || one != want {
		t.Errorf("/v1/models/swift = %d %+v", c, one)
	}
}

// Without detection the fields are absent, not 0.
func TestModelsCacheSlotsOmitted(t *testing.T) {
	up := newGLMUpstream(t)
	h, _ := capRig(t, capStore(t), map[string]config.Model{"swift": modelTo(t, up.srv.URL, "chat")})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if strings.Contains(rec.Body.String(), "cache_slots") {
		t.Errorf("cache_slots present without detection: %s", rec.Body)
	}
}
