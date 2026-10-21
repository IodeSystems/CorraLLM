package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/store"
)

// A request that queued in the predecessor before the handover is logged by
// the successor with the WHOLE wait: the predecessor's share is carried on the
// forward and added to dwell and queued. Before, the row counted only the
// successor's few milliseconds (live: ~27 s in the old process, gone).
func TestHandoff_ForwardCarriesPredecessorWait(t *testing.T) {
	treatAsRemote(t)
	release := make(chan struct{})
	started := make(chan struct{}, 4)
	oldUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		started <- struct{}{}
		<-release
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"from old"}}]}`))
	}))
	t.Cleanup(oldUp.Close)
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	leaked := make(chan string, 4)
	newUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked <- r.Header.Get(HandoffTokenHeader) + r.Header.Get(HandoffWaitedHeader)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"from successor"}}]}`))
	}))
	t.Cleanup(newUp.Close)

	m := modelTo(t, oldUp.URL, "chat")
	m.MaxConcurrent = 1
	hOld, pxOld, scOld, _ := handoffRig(t, map[string]config.Model{"m": m})
	hNew, pxNew, _, stNew := handoffRig(t, map[string]config.Model{"m": modelTo(t, newUp.URL, "chat")})
	pxNew.SetPredecessorToken("tok")
	succ := httptest.NewServer(hNew)
	t.Cleanup(succ.Close)

	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- post(hOld, "m") }()
	<-started
	second := make(chan *httptest.ResponseRecorder, 1)
	go func() { second <- post(hOld, "m") }()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if snap := scOld.Snapshot(); len(snap.Backends) == 1 && snap.Backends[0].Waiting == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	const queuedFor = 400 * time.Millisecond
	time.Sleep(queuedFor)

	if err := pxOld.SetSuccessor(succ.URL, "tok"); err != nil {
		t.Fatal(err)
	}
	scOld.Drain()
	select {
	case rec := <-second:
		if !strings.Contains(rec.Body.String(), "from successor") {
			t.Fatalf("queued request: %d %q", rec.Code, rec.Body.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("queued request was not handed over")
	}
	if got := <-leaked; got != "" {
		t.Errorf("handoff token/wait reached the backend: %q", got)
	}
	rows, err := stNew.RecentActivity(store.Window{}, 10, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("successor rows %d, want 1", len(rows))
	}
	if r := rows[0]; r.QueuedMS < queuedFor.Milliseconds() || r.DwellMS < queuedFor.Milliseconds() {
		t.Errorf("successor row queued %d ms dwell %d ms; want both >= %d ms (the predecessor's wait)",
			r.QueuedMS, r.DwellMS, queuedFor.Milliseconds())
	}
}

// The wait header is believed only with the handoff's token: the forwards
// arrive on the public port, so anyone can send the headers.
func TestHandoff_WaitNeedsToken(t *testing.T) {
	treatAsRemote(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	t.Cleanup(up.Close)
	for _, tc := range []struct {
		name, token string
		want        bool
	}{
		{"wrong token", "nope", false},
		{"no token", "", false},
		{"right token", "tok", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, px, _, st := handoffRig(t, map[string]config.Model{"m": modelTo(t, up.URL, "chat")})
			px.SetPredecessorToken("tok")
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
				strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
			req.Header.Set("X-Corrallm-Key", "k")
			req.Header.Set(HandoffHeader, "1")
			req.Header.Set(HandoffWaitedHeader, "90000")
			if tc.token != "" {
				req.Header.Set(HandoffTokenHeader, tc.token)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != 200 {
				t.Fatalf("status %d %q", rec.Code, rec.Body.String())
			}
			rows, err := st.RecentActivity(store.Window{}, 10, "", "", "")
			if err != nil || len(rows) != 1 {
				t.Fatalf("rows %d err %v", len(rows), err)
			}
			if got := rows[0].QueuedMS >= 90000 && rows[0].DwellMS >= 90000; got != tc.want {
				t.Errorf("queued %d dwell %d; wait believed=%v, want %v", rows[0].QueuedMS, rows[0].DwellMS, got, tc.want)
			}
		})
	}
}
