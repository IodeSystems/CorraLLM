package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/proc"
	"github.com/iodesystems/corrallm/internal/sched"
	"github.com/iodesystems/corrallm/internal/store"
)

// busyProxy stands a proxy up with its single slot already occupied, so the
// next request is guaranteed to be turned away.
func busyProxy(t *testing.T) (*chi.Mux, *store.Store, func()) {
	t.Helper()
	started := make(chan struct{})
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			return
		}
		select {
		case <-started:
		default:
			close(started)
		}
		<-release
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))

	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	mgr := proc.NewManager(&config.Config{})
	r := chi.NewRouter()
	New(mkConfig(t, "mock", upstream.URL), mgr, sched.New(), st).Mount(r)

	go func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"mock"}`))
		r.ServeHTTP(httptest.NewRecorder(), req)
	}()
	<-started

	return r, st, func() {
		close(release)
		mgr.Shutdown()
		_ = st.Close()
		upstream.Close()
	}
}

func ticketOf(t *testing.T, rec *httptest.ResponseRecorder) (header string, body string) {
	t.Helper()
	var out struct {
		Error struct {
			Ticket string `json:"ticket"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("429 body is not JSON: %s", rec.Body.String())
	}
	return rec.Header().Get(HeaderTicket), out.Error.Ticket
}

// A rejected caller that had no id of its own leaves with one, in both places,
// and the row we logged carries the SAME id — otherwise the ticket names a
// journey the log cannot find.
func TestRejectionMintsATicketAndRecordsIt(t *testing.T) {
	r, st, done := busyProxy(t)
	defer done()

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"mock"}`)))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("want 429, got %d (%s)", rec.Code, rec.Body.String())
	}

	hdr, body := ticketOf(t, rec)
	if hdr == "" {
		t.Error("no ticket header on the rejection; the caller has nothing to send back")
	}
	if hdr != body {
		t.Errorf("header ticket %q and body ticket %q disagree", hdr, body)
	}
	if !strings.HasPrefix(hdr, ticketPrefix) {
		t.Errorf("minted ticket %q does not carry our prefix", hdr)
	}

	acts, err := st.RecentActivity(store.Window{}, 10, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, a := range acts {
		if a.Status == http.StatusTooManyRequests {
			found = true
			if a.Ticket != hdr {
				t.Errorf("logged ticket %q, handed the caller %q", a.Ticket, hdr)
			}
		}
	}
	if !found {
		t.Fatalf("no 429 row logged: %+v", acts)
	}
}

// A caller that already has an id keeps it. Minting over the caller's own id
// would break the correlation on their side, which is the whole point of
// accepting one.
func TestACallersOwnRequestIDIsKept(t *testing.T) {
	r, st, done := busyProxy(t)
	defer done()

	const mine = "req-from-the-caller-123"
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"mock"}`))
	req.Header.Set(HeaderRequestIDAlt, mine) // the ecosystem spelling
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	hdr, body := ticketOf(t, rec)
	if hdr != mine || body != mine {
		t.Errorf("caller id %q was replaced (header %q, body %q)", mine, hdr, body)
	}
	acts, _ := st.RecentActivity(store.Window{}, 10, "", "", "")
	for _, a := range acts {
		if a.Status == http.StatusTooManyRequests && a.Ticket != mine {
			t.Errorf("logged ticket %q, want the caller's own %q", a.Ticket, mine)
		}
	}
}

// Age is what a later scheduling decision would spend, so it must be OURS to
// grant: signed by this process, never in the future, and never inferred from
// an id somebody else chose.
func TestTicketAgeOnlyTrustsOurOwnSignedTickets(t *testing.T) {
	now := time.Now()
	minted := mintTicket(now.Add(-30 * time.Second))

	age, ok := ticketAge(minted, now)
	if !ok {
		t.Fatal("our own freshly minted ticket did not verify")
	}
	if age < 29*time.Second || age > 31*time.Second {
		t.Errorf("age = %s, want ~30s", age)
	}

	for name, tk := range map[string]string{
		"a caller's own id":   "req-from-the-caller-123",
		"empty":               "",
		"wrong shape":         ticketPrefix + "nope",
		"forged signature":    ticketPrefix + "abc_def_000000000000",
		"claims to be future": mintTicket(now.Add(time.Minute)),
	} {
		if _, ok := ticketAge(tk, now); ok {
			t.Errorf("%s: granted age credit", name)
		}
	}
}

// A minted ticket is unique per rejection: two callers turned away in the same
// millisecond must not end up sharing a journey.
func TestMintedTicketsAreDistinct(t *testing.T) {
	now := time.Now()
	seen := map[string]bool{}
	for range 100 {
		tk := mintTicket(now)
		if seen[tk] {
			t.Fatalf("duplicate ticket %q minted within one millisecond", tk)
		}
		seen[tk] = true
	}
}

// The join between the two halves: our own ticket buys scheduling age, anything
// else buys nothing. If this seam breaks, priority silently becomes a header
// anyone can write — or patience stops counting at all, with every unit test on
// either side still passing.
func TestOnlyOurTicketBuysSchedulingAge(t *testing.T) {
	mine := mintTicket(time.Now().Add(-40 * time.Second))

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set(HeaderRequestID, mine)
	ctx := agedCtx(context.Background(), req)
	since := sched.WaitingSinceForTest(ctx)
	if since.IsZero() {
		t.Fatal("our own ticket did not reach the scheduler as an attempt start")
	}
	if age := time.Since(since); age < 39*time.Second || age > 41*time.Second {
		t.Errorf("attempt start implies age %s, want ~40s", age)
	}

	for name, hdr := range map[string]string{
		"a caller's own id": "req-abc",
		"a forged ticket":   ticketPrefix + "abc_def_000000000000",
	} {
		r2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		r2.Header.Set(HeaderRequestID, hdr)
		if !sched.WaitingSinceForTest(agedCtx(context.Background(), r2)).IsZero() {
			t.Errorf("%s bought scheduling age", name)
		}
	}
	// And a request with no ticket at all is untouched.
	bare := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if !sched.WaitingSinceForTest(agedCtx(context.Background(), bare)).IsZero() {
		t.Error("a request with no ticket was aged")
	}
}
