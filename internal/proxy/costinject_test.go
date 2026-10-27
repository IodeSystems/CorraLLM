package proxy

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/proc"
	"github.com/iodesystems/corrallm/internal/sched"
	"github.com/iodesystems/corrallm/internal/store"
)

// costCase runs one request through the proxy against a backend answering
// with ct and body, and returns what the caller got and what the row recorded.
func costCase(t *testing.T, cfg func(*config.Config), reqBody, ct string, status int, body ...string) (*httptest.ResponseRecorder, store.Activity) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			return
		}
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(status)
		f, _ := w.(http.Flusher)
		for _, b := range body {
			_, _ = w.Write([]byte(b))
			if f != nil {
				f.Flush()
			}
		}
	}))
	defer upstream.Close()

	st, _ := store.Open(context.Background(), ":memory:")
	defer func() { _ = st.Close() }()
	mgr := proc.NewManager(&config.Config{})
	defer mgr.Shutdown()

	c := meteredConfig(t, "mock", upstream.URL)
	if cfg != nil {
		cfg(c)
	}
	r := chi.NewRouter()
	New(c, mgr, sched.New(), st).Mount(r)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody)))
	acts, _ := st.RecentActivity(store.Window{}, 1, "", "", "")
	if len(acts) != 1 {
		t.Fatalf("want 1 activity, got %d", len(acts))
	}
	return rec, acts[0]
}

type costUsage struct {
	Cost       *float64 `json:"cost"`
	CostSource string   `json:"cost_source"`
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-12 }

// TestCostInjectedNonStreaming: the caller's usage carries the same $ the row
// records, labelled as this box's energy estimate.
func TestCostInjectedNonStreaming(t *testing.T) {
	rec, a := costCase(t, nil, `{"model":"mock"}`, "application/json", 200,
		`{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	var r struct {
		Usage costUsage `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("body not JSON: %v: %s", err, rec.Body.String())
	}
	if r.Usage.Cost == nil || !near(*r.Usage.Cost, a.CostUSD) || a.CostUSD <= 0 {
		t.Errorf("cost = %v, row %v: %s", r.Usage.Cost, a.CostUSD, rec.Body.String())
	}
	if r.Usage.CostSource != costFromEnergy {
		t.Errorf("source = %q, want %q", r.Usage.CostSource, costFromEnergy)
	}
	if cl := rec.Header().Get("Content-Length"); cl != "" && cl != itoa(rec.Body.Len()) {
		t.Errorf("Content-Length %s describes the backend's body, sent %d", cl, rec.Body.Len())
	}
}

// TestCostInjectedStreaming: only the usage event changes; deltas and the
// terminator pass through as sent.
func TestCostInjectedStreaming(t *testing.T) {
	delta := `data: {"choices":[{"delta":{"content":"x"}}]}` + "\n\n"
	rec, a := costCase(t, nil, `{"model":"mock","stream":true}`, "text/event-stream", 200,
		delta, delta,
		`data: {"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":7}}`+"\n", "\n",
		"data: [DONE]\n\n")
	got := rec.Body.String()
	if strings.Count(got, delta) != 2 || !strings.HasSuffix(got, "data: [DONE]\n\n") {
		t.Errorf("stream changed outside usage: %q", got)
	}
	var found bool
	for _, line := range strings.Split(got, "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok || !strings.Contains(data, "usage") {
			continue
		}
		var r struct {
			Usage costUsage `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &r); err != nil {
			t.Fatalf("usage event not JSON: %v: %s", err, data)
		}
		found = r.Usage.Cost != nil && near(*r.Usage.Cost, a.CostUSD) && r.Usage.CostSource == costFromEnergy
	}
	if !found || a.CostUSD <= 0 {
		t.Errorf("no usage event with cost %v: %q", a.CostUSD, got)
	}
}

// TestCostInjectedLargeBody: a body bigger than the held window streams
// through whole, and the usage at its end still gets the cost.
func TestCostInjectedLargeBody(t *testing.T) {
	big := strings.Repeat("0.123,", 3*costWindow/6)
	rec, a := costCase(t, nil, `{"model":"mock"}`, "application/json", 200,
		`{"data":[{"embedding":[`+big, `1]}],`, `"usage":{"prompt_tokens":40,"total_tokens":40}}`)
	var r struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
		Usage costUsage `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if len(r.Data) != 1 || len(r.Data[0].Embedding) != 3*costWindow/6+1 {
		t.Errorf("embedding damaged")
	}
	if r.Usage.Cost == nil || !near(*r.Usage.Cost, a.CostUSD) {
		t.Errorf("cost = %v, row %v", r.Usage.Cost, a.CostUSD)
	}
}

// TestCostInjectedKeepsProviderCost: an upstream's own usage.cost is the
// figure; corrallm only says where it came from.
func TestCostInjectedKeepsProviderCost(t *testing.T) {
	rec, a := costCase(t, nil, `{"model":"mock"}`, "application/json", 200,
		`{"usage":{"prompt_tokens":10,"completion_tokens":5,"cost":0.0042}}`)
	if got := rec.Body.String(); strings.Count(got, `"cost":`) != 1 || !strings.Contains(got, `"cost_source":"provider"`) {
		t.Errorf("body = %s", got)
	}
	if !near(a.CostUSD, 0.0042) {
		t.Errorf("row cost = %v, want the provider's 0.0042", a.CostUSD)
	}
}

// TestCostNotInjectedIntoErrors: an error body leaves as the backend sent it.
func TestCostNotInjectedIntoErrors(t *testing.T) {
	const body = `{"error":{"message":"bad"},"usage":{"prompt_tokens":1}}`
	rec, _ := costCase(t, nil, `{"model":"mock"}`, "application/json", 400, body)
	if rec.Body.String() != body {
		t.Errorf("error body changed: %s", rec.Body.String())
	}
}

func TestWithCostEmptyUsage(t *testing.T) {
	c := &costWriter{price: func(usage) (float64, string) { return 0.5, costFromPrices }}
	got := string(c.withCost([]byte(`{"usage":{}}`), usage{}))
	if got != `{"usage":{"cost":0.5,"cost_source":"price-table"}}` {
		t.Errorf("got %s", got)
	}
}

// TestWithStreamUsage: a stream that did not say gets the usage chunk asked
// for; one that said either way, or a non-stream, is left as sent.
func TestWithStreamUsage(t *testing.T) {
	for _, c := range []struct {
		in   string
		want bool // rewritten
	}{
		{`{"model":"m","stream":true}`, true},
		{`{"model":"m","stream":true,"stream_options":{"foo":1}}`, true},
		{`{"model":"m","stream":true,"stream_options":{"include_usage":false}}`, false},
		{`{"model":"m","stream":true,"stream_options":{"include_usage":true}}`, false},
		{`{"model":"m"}`, false},
		{`not json`, false},
	} {
		out, did := withStreamUsage([]byte(c.in))
		if did != c.want {
			t.Errorf("%s: rewritten=%v, want %v", c.in, did, c.want)
			continue
		}
		if did {
			var r struct {
				StreamOptions map[string]any `json:"stream_options"`
			}
			if err := json.Unmarshal(out, &r); err != nil || r.StreamOptions["include_usage"] != true {
				t.Errorf("%s → %s", c.in, out)
			}
			if strings.Contains(c.in, "foo") && r.StreamOptions["foo"] == nil {
				t.Errorf("other stream_options dropped: %s", out)
			}
		}
	}
}
