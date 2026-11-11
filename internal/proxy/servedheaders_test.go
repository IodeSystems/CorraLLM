package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/sched"
	"github.com/iodesystems/corrallm/internal/store"
)

// A client running an A/B on a lane needs, per reply, which name it asked for
// and which model answered — and an id that finds the row. These are those
// three headers.

func rowsOf(t *testing.T, st *store.Store) []store.Activity {
	t.Helper()
	rows, err := st.RecentActivity(store.Window{}, 10, "", "", "")
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	return rows
}

func TestServedHeadersOnABufferedLaneReply(t *testing.T) {
	r, st := laneRig(t)
	rec := askFor(t, r, "chat")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get(HeaderRequested); got != "chat" {
		t.Errorf("%s = %q, want the lane asked for (chat)", HeaderRequested, got)
	}
	if got := rec.Header().Get(HeaderServed); got != "m" {
		t.Errorf("%s = %q, want the member that answered (m)", HeaderServed, got)
	}
	id := rec.Header().Get(HeaderRequestID)
	if !strings.HasPrefix(id, ticketPrefix) {
		t.Fatalf("%s = %q, want a minted id", HeaderRequestID, id)
	}
	if row := onlyRow(t, st); row.Ticket != id {
		t.Errorf("row ticket = %q, reply named %q: the reply cannot find its row", row.Ticket, id)
	}
}

func TestServedHeadersOnAStreamedReply(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
		w.(http.Flusher).Flush()
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(up.Close)
	h, st := refusalRig(t, map[string]config.Model{"m": modelTo(t, up.URL, "chat")},
		map[string]config.Lane{"ab": {Members: []config.LaneMember{{Model: "m"}}}})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"ab","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("X-Corrallm-Key", "k")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatalf("status = %d body = %q, want a 200 stream", rec.Code, rec.Body.String())
	}
	if rec.Header().Get(HeaderRequested) != "ab" || rec.Header().Get(HeaderServed) != "m" {
		t.Errorf("requested/served = %q/%q, want ab/m", rec.Header().Get(HeaderRequested), rec.Header().Get(HeaderServed))
	}
	id := rec.Header().Get(HeaderRequestID)
	rows := rowsOf(t, st)
	if id == "" || len(rows) != 1 || rows[0].Ticket != id {
		t.Errorf("id %q, rows %+v: want one row carrying the id", id, rows)
	}
}

// /v1/messages goes through the same walk; translated to an OpenAI backend,
// the headers still name the lane and the member.
func TestServedHeadersOnMessages(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","model":"upstream-name","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(up.Close)
	h, _ := refusalRig(t, map[string]config.Model{"m": modelTo(t, up.URL, "chat")},
		map[string]config.Lane{"ab": {Members: []config.LaneMember{{Model: "m"}}}})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"ab","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("X-Corrallm-Key", "k")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	if rec.Header().Get(HeaderRequested) != "ab" || rec.Header().Get(HeaderServed) != "m" ||
		rec.Header().Get(HeaderRequestID) == "" {
		t.Errorf("headers = %v, want requested ab, served m and an id", rec.Header())
	}
}

// Failover: the member that refused must not be named; the one that answered
// must, and both rows of the journey carry the same id.
func TestServedHeaderNamesTheMemberThatAnsweredAfterFailover(t *testing.T) {
	treatAsRemote(t)
	shed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(shed.Close)
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(ok.Close)
	a, b := modelTo(t, shed.URL, "chat"), modelTo(t, ok.URL, "chat")
	a.Quality, b.Quality = 1, 1
	h, st := refusalRig(t, map[string]config.Model{"a": a, "b": b},
		map[string]config.Lane{"chat": {Members: []config.LaneMember{{Model: "a"}, {Model: "b"}}}})

	rec := post(h, "chat")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200 from b", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Values(HeaderServed); len(got) != 1 || got[0] != "b" {
		t.Errorf("%s = %v, want exactly [b]", HeaderServed, got)
	}
	if got := rec.Header().Get(HeaderRequested); got != "chat" {
		t.Errorf("%s = %q, want chat", HeaderRequested, got)
	}
	id := rec.Header().Get(HeaderRequestID)
	rows := rowsOf(t, st)
	if len(rows) != 2 {
		t.Fatalf("want 2 rows (refusal + answer), got %+v", rows)
	}
	for _, row := range rows {
		if row.Ticket != id {
			t.Errorf("row %s/%d ticket %q, reply named %q", row.Served, row.Status, row.Ticket, id)
		}
	}
}

// The caller's id is kept and stored; ours is never sent to a backend; an
// upstream corrallm's copies of the headers do not double ours up.
func TestRequestIDHeaderKeepsTheCallersOwn(t *testing.T) {
	var sawID []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawID = append(sawID, r.Header.Get(HeaderRequestID))
		// What a corrallm upstream would send back.
		w.Header().Set(HeaderRequested, "inner")
		w.Header().Set(HeaderRequestID, "inner-id")
		w.Header().Set(HeaderServed, "inner-model")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(up.Close)
	h, st := refusalRig(t, map[string]config.Model{"m": modelTo(t, up.URL, "chat")}, nil)

	const mine = "caller-req-42"
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("X-Corrallm-Key", "k")
	req.Header.Set(HeaderRequestIDAlt, mine)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	for hdr, want := range map[string]string{HeaderRequestID: mine, HeaderRequested: "m", HeaderServed: "m"} {
		if got := rec.Header().Values(hdr); len(got) != 1 || got[0] != want {
			t.Errorf("%s = %v, want exactly [%s]", hdr, got, want)
		}
	}
	if row := onlyRow(t, st); row.Ticket != mine {
		t.Errorf("row ticket = %q, want the caller's %q", row.Ticket, mine)
	}

	// No id sent: one is minted, and it stays in corrallm.
	rec = post(h, "m")
	if rec.Header().Get(HeaderRequestID) == "" {
		t.Fatal("no id minted")
	}
	if len(sawID) != 2 || sawID[1] != "" {
		t.Errorf("upstream saw ids %q; a minted id must not be forwarded", sawID)
	}
}

// A refusal corrallm writes itself still names what was asked for and the id,
// and names no served model.
func TestRefusalCarriesRequestedAndIDButNoServed(t *testing.T) {
	r, st := laneRig(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"pub"}`))
	req.Header.Set("X-Corrallm-Key", "yscr")
	req.Header.Set("X-Corrallm-Sensitive", "true")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if rec.Header().Get(HeaderRequested) != "pub" || rec.Header().Get(HeaderServed) != "" {
		t.Errorf("requested/served = %q/%q, want pub/(absent)", rec.Header().Get(HeaderRequested), rec.Header().Get(HeaderServed))
	}
	if id := rec.Header().Get(HeaderRequestID); id == "" || onlyRow(t, st).Ticket != id {
		t.Errorf("id %q not on the row", id)
	}
}

// An id minted on arrival buys no scheduling age: before it existed such a
// request was not aged at all, and fairshare must not change because of it.
func TestIDMintedOnArrivalBuysNoAge(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req, id := stampRequestID(req)
	if !strings.HasPrefix(id, ticketPrefix) {
		t.Fatalf("minted %q", id)
	}
	if !sched.WaitingSinceForTest(agedCtx(context.Background(), req)).IsZero() {
		t.Error("an id minted for this request aged it")
	}
	// Echoed on a later attempt, the same id is the caller's and does age.
	retry := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	retry.Header.Set(HeaderRequestID, id)
	retry, got := stampRequestID(retry)
	if got != id {
		t.Errorf("echoed id replaced: %q -> %q", id, got)
	}
	if sched.WaitingSinceForTest(agedCtx(context.Background(), retry)).IsZero() {
		t.Error("an echoed ticket lost its age credit")
	}
}
