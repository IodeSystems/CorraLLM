package proxy

import (
	"bufio"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/store"
)

// An agent client stops reading at the chunk carrying finish_reason and usage.
// A provider that sends [DONE] after that makes the proxy's next write fail,
// and ReverseProxy aborts the handler. The request was served and billed
// upstream, so it must still be metered and logged.
func TestCallerHangingUpAfterUsageIsStillLogged(t *testing.T) {
	treatAsRemote(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"}}]}\n\n")
		fl.Flush()
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5}}\n\n")
		fl.Flush()
		time.Sleep(300 * time.Millisecond) // [DONE] arrives after the caller left
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	t.Cleanup(up.Close)
	h, st := refusalRig(t, map[string]config.Model{"fw": modelTo(t, up.URL, "chat")}, nil)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"fw","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("X-Corrallm-Key", "k")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if strings.Contains(sc.Text(), `"usage"`) {
			break
		}
	}
	_ = resp.Body.Close()

	var rows []store.Activity
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if rows, _ = st.RecentActivity(store.Window{}, 10, "", "", ""); len(rows) > 0 {
			break
		}
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1: a served request vanished from the log", len(rows))
	}
	r := rows[0]
	if r.Status != 200 || r.PromptTokens != 10 || r.CompletionTokens != 5 || !strings.Contains(r.Error, "hung up") {
		t.Errorf("row = status %d, %d/%d tokens, error %q", r.Status, r.PromptTokens, r.CompletionTokens, r.Error)
	}
}

// /upstream/ skips allowlists, limits and the log, so a remote model is refused.
func TestUpstreamDoorRefusesARemoteModel(t *testing.T) {
	treatAsRemote(t)
	hit := false
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hit = true }))
	t.Cleanup(up.Close)
	h, _ := refusalRig(t, map[string]config.Model{"fw": modelTo(t, up.URL, "chat")}, nil)
	req := httptest.NewRequest(http.MethodPost, "/upstream/fw/v1/chat/completions", strings.NewReader(`{}`))
	req.Header.Set("X-Corrallm-Key", "k")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || hit {
		t.Errorf("status %d, upstream reached %v; want 403 and no forward", rec.Code, hit)
	}
}
