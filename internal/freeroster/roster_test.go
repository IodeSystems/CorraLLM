package freeroster

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// FetchFree extracts free ids by :free suffix OR pricing 0/0, over a real HTTP
// round-trip, and passes the auth header.
func TestFetchFree(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte(`{"data":[
			{"id":"paid/model","pricing":{"prompt":"0.001","completion":"0.002"}},
			{"id":"nvidia/nemotron:free","pricing":{"prompt":"0","completion":"0"}},
			{"id":"gemma-31b","pricing":{"prompt":"0","completion":"0"}}
		]}`))
	}))
	defer srv.Close()

	free, err := FetchFree(context.Background(), srv.Client(), srv.URL+"/v1/models",
		map[string]string{"authorization": "Bearer k"})
	if err != nil {
		t.Fatal(err)
	}
	if len(free) != 2 {
		t.Fatalf("want 2 free (suffix + pricing-0), got %v", free)
	}
	set := map[string]bool{}
	for _, id := range free {
		set[id] = true
	}
	if !set["nvidia/nemotron:free"] || !set["gemma-31b"] || set["paid/model"] {
		t.Errorf("wrong free set: %v", free)
	}
	if gotAuth != "Bearer k" {
		t.Errorf("auth header not sent: %q", gotAuth)
	}
}

func TestFetchFree_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()
	if _, err := FetchFree(context.Background(), srv.Client(), srv.URL+"/v1/models", nil); err == nil {
		t.Error("a 500 must be an error")
	}
}

// Has distinguishes confirmed-gone from never-fetched, and a fetch error leaves
// the prior set intact.
func TestRoster_HasAndErrorPreservesSet(t *testing.T) {
	r := New()
	now := time.Unix(1_800_000_000, 0)

	// Never fetched → not known.
	if _, known := r.Has("openrouter", "x:free"); known {
		t.Error("unfetched provider should be unknown")
	}

	r.Set("openrouter", []string{"a:free", "b:free"}, nil, now)
	if free, known := r.Has("openrouter", "a:free"); !known || !free {
		t.Error("a:free should be known-free")
	}
	if free, known := r.Has("openrouter", "gone:free"); !known || free {
		t.Error("gone:free should be known-and-absent")
	}

	// A fetch error must NOT wipe the last-good set (transient failure).
	r.Set("openrouter", nil, context.DeadlineExceeded, now.Add(time.Minute))
	if free, known := r.Has("openrouter", "a:free"); !known || !free {
		t.Error("a transient fetch error must preserve the prior roster")
	}
	// The error is surfaced in the snapshot.
	if s := r.Snapshot(); len(s) != 1 || s[0].Error == "" {
		t.Errorf("snapshot should carry the error: %+v", s)
	}
}

// Context windows and prices come from whichever vendor field is present.
func TestParseCatalogVendorShapes(t *testing.T) {
	cases := []struct {
		name, body string
		ctx        int
		prompt     float64
		label      string
	}{
		{"groq", `{"data":[{"id":"m","context_window":131072}]}`, 131072, 0, ""},
		{"vllm", `{"data":[{"id":"m","max_model_len":32768}]}`, 32768, 0, ""},
		{"anthropic", `{"data":[{"id":"claude-x","display_name":"Claude X","max_input_tokens":200000}]}`, 200000, 0, "Claude X"},
		{"together bare array", `[{"id":"m","display_name":"M","context_length":8192,"pricing":{"input":0.6,"output":1.2}}]`, 8192, 0.6 / 1e6, "M"},
		{"openrouter", `{"data":[{"id":"m","name":"M","context_length":4096,"pricing":{"prompt":"0.000001","completion":"0.000002"}}]}`, 4096, 0.000001, "M"},
	}
	for _, c := range cases {
		got, err := parseCatalog([]byte(c.body))
		if err != nil || len(got) != 1 {
			t.Fatalf("%s: %v %v", c.name, got, err)
		}
		e := got[0]
		if e.ContextLength != c.ctx || e.PromptUSD != c.prompt || e.Name != c.label || e.Free {
			t.Errorf("%s: %+v", c.name, e)
		}
	}
	// A listed zero is free; absent pricing is not.
	got, _ := parseCatalog([]byte(`{"data":[{"id":"a","pricing":{"prompt":"0","completion":"0"}},{"id":"b"}]}`))
	if !got[0].Free || got[1].Free {
		t.Errorf("free: %+v", got)
	}
}
