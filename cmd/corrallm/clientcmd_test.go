package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestParseBudgetsAndModels(t *testing.T) {
	b, err := parseBudgets([]string{"*=$20/calendar-day", "model:*deepseek*=$3/calendar-day,hard", "*=1000req/hour", "fw-ds=$1/day"})
	if err != nil {
		t.Fatal(err)
	}
	if len(b["*"]) != 2 || !b["model:*deepseek*"][0].Hard || len(b["model:fw-ds"]) != 1 {
		t.Errorf("budgets = %+v", b)
	}
	for _, bad := range []string{"$3/day", "=$3/day", "*=3/day"} {
		if _, err := parseBudgets([]string{bad}); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
	m, err := parseModels([]string{"local,provider:fireworks", "fw-ds"})
	if err != nil || strings.Join(m, " ") != "local model:fw-ds provider:fireworks" {
		t.Errorf("models = %v %v", m, err)
	}
	v, err := parseValueAdd("$10@deepseek,2d")
	if err != nil || v["usd"] != 10.0 || v["model"] != "deepseek" || v["ttl"] != "2d" {
		t.Errorf("value add = %v %v", v, err)
	}
}

// fakeServer records what the CLI sent and answers like corrallm.
type fakeServer struct {
	mu    sync.Mutex
	seen  []string
	polls int
}

func (f *fakeServer) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.seen = append(f.seen, r.Method+" "+r.URL.Path+" auth="+r.Header.Get("Authorization")+" "+string(body))
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/access-requests":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"ar-1","claim":"cl-secret","status":"pending"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/access-requests/ar-1":
			if r.Header.Get("Authorization") != "Bearer cl-secret" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			f.mu.Lock()
			f.polls++
			n := f.polls
			f.mu.Unlock()
			if n < 2 {
				_, _ = w.Write([]byte(`{"id":"ar-1","status":"pending"}`))
				return
			}
			_, _ = w.Write([]byte(`{"id":"ar-1","status":"approved","key":"ck-new","granted":{"budgets":{"*":[{"usd":5,"per":"calendar-day"}]}}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/keys":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"key":"ck-kid","label":"proj","models":["local"],"budgets":{"*":[{"usd":2,"per":"calendar-day","hard":true}]},` +
				`"windows":{"*":[{"label":"usd/calendar-day","limit":2,"used":0.5}]},"warnings":["subkeys' budgets add up"]}`))
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/v1/keys/"):
			_, _ = w.Write([]byte(`{"key":"ck-kid","label":"proj","models":[],"budgets":{},"disabled":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"message":"no such thing"}}`))
		}
	})
}

func runCLI(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	root := newRoot()
	var out, errb bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errb)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), errb.String(), err
}

func TestRequestNewWaitPrintsTheKeyAndSavesTheClaim(t *testing.T) {
	f := &fakeServer{}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	home := t.TempDir()
	t.Setenv("CORRALLM_KEY", "sk-should-not-be-sent")
	out, _, err := runCLI(t, "request", "new", "--url", srv.URL, "--home", home, "--label", "bench",
		"--purpose", "runs", "--models", "local", "--budget", "*=$5/calendar-day", "--wait", "--interval", "1ms")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != "ck-new" {
		t.Errorf("stdout = %q, want the key alone", out)
	}
	if !strings.Contains(f.seen[0], "auth= ") || !strings.Contains(f.seen[0], `"budgets":{"*":[`) ||
		!strings.Contains(f.seen[0], `"models":["local"]`) || strings.Contains(f.seen[0], "mint") {
		t.Errorf("filed %s", f.seen[0])
	}
	st, err := os.Stat(filepath.Join(home, "requests", "ar-1.json"))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("saved file: %v %v", st, err)
	}
	r, _ := loadRequest(home, "ar-1")
	if r.Claim != "cl-secret" || r.Key != "ck-new" || r.Status != "approved" {
		t.Errorf("saved = %+v", r)
	}
	out, _, err = runCLI(t, "request", "status", "--home", home)
	if err != nil || !strings.Contains(out, "ar-1  approved") || !strings.Contains(out, "budgets: * $5/calendar-day") {
		t.Errorf("status = %q %v", out, err)
	}
}

func TestRequestMoreSendsTheKey(t *testing.T) {
	f := &fakeServer{}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	t.Setenv("CORRALLM_KEY", "sk-mine")
	if _, _, err := runCLI(t, "request", "more", "--url", srv.URL, "--home", t.TempDir(),
		"--purpose", "more", "--value-add", "$10@ds,2d"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.seen[0], "auth=Bearer sk-mine") || !strings.Contains(f.seen[0], `"valueAdds":[{"model":"ds","ttl":"2d","usd":10}]`) ||
		strings.Contains(f.seen[0], `"budgets"`) || strings.Contains(f.seen[0], `"label"`) {
		t.Errorf("filed %s", f.seen[0])
	}
}

func TestKeysMintAndSet(t *testing.T) {
	f := &fakeServer{}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	t.Setenv("CORRALLM_KEY", "sk-parent")
	out, errOut, err := runCLI(t, "keys", "mint", "--url", srv.URL, "--label", "proj", "--models", "local",
		"--budget", "*=$2/calendar-day,hard", "--ttl", "30d")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != "ck-kid" || !strings.Contains(errOut, "$0.50 of $2.00 used") || !strings.Contains(errOut, "warning:") {
		t.Errorf("stdout %q stderr %q", out, errOut)
	}
	var sent map[string]any
	_ = json.Unmarshal([]byte(f.seen[0][strings.Index(f.seen[0], "{"):]), &sent)
	if sent["ttl"] != "30d" || sent["label"] != "proj" || sent["budgets"] == nil {
		t.Errorf("mint sent %v", sent)
	}
	if _, _, err := runCLI(t, "keys", "set", "ck-kid", "--url", srv.URL, "--all-models", "--no-budgets"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.seen[1], `PATCH /v1/keys/ck-kid`) || !strings.Contains(f.seen[1], `"budgets":{}`) || !strings.Contains(f.seen[1], `"models":[]`) {
		t.Errorf("set sent %s", f.seen[1])
	}
	if _, _, err := runCLI(t, "keys", "set", "ck-kid", "--url", srv.URL); err == nil {
		t.Error("set with nothing to change succeeded")
	}
	t.Setenv("CORRALLM_KEY", "")
	if _, _, err := runCLI(t, "keys", "list", "--url", srv.URL); err == nil || !strings.Contains(err.Error(), "CORRALLM_KEY") {
		t.Errorf("no key: %v", err)
	}
}
