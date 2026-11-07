package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/store"
)

func fakeUpstream(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(up.Close)
	return up
}

func TestUpstreamFundsRefusalIs402(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		upMsg  string
	}{
		{"402", 402, `{"error":{"message":"Account suspended: no credits"}}`, "Account suspended: no credits"},
		{"openai insufficient_quota", 429, `{"error":{"message":"You exceeded your current quota","type":"insufficient_quota","code":"insufficient_quota"}}`, "You exceeded your current quota"},
		{"anthropic credit", 400, `{"type":"error","error":{"type":"invalid_request_error","message":"Your credit balance is too low to access the Anthropic API."}}`, "Your credit balance is too low to access the Anthropic API."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			treatAsRemote(t)
			up := fakeUpstream(t, c.status, c.body)
			h, st := refusalRig(t, map[string]config.Model{"fw": modelTo(t, up.URL, "chat")}, nil)
			rec := post(h, "fw")
			if rec.Code != http.StatusPaymentRequired {
				t.Fatalf("status = %d (%s), want 402", rec.Code, rec.Body)
			}
			if rec.Header().Get("Retry-After") != "" {
				t.Errorf("Retry-After = %q, want none", rec.Header().Get("Retry-After"))
			}
			var got struct {
				Error struct {
					Message  string `json:"message"`
					Type     string `json:"type"`
					Provider string `json:"provider"`
					Up       int    `json:"upstream_status"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("body %q: %v", rec.Body, err)
			}
			e := got.Error
			if e.Type != "upstream_funds_exhausted" || e.Provider != "fw" || e.Up != c.status ||
				!strings.HasPrefix(e.Message, "not your balance: the fw account") || !strings.Contains(e.Message, c.upMsg) {
				t.Errorf("error = %+v", e)
			}
			rows, _ := st.RecentActivity(store.Window{}, 10, "", "", "")
			if len(rows) != 1 || rows[0].Status != 402 || !strings.Contains(rows[0].Error, "out of funds") {
				t.Errorf("rows = %+v, want one 402 row naming the funds", rows)
			}
		})
	}
}

// An ordinary rate limit is not a funds refusal.
func TestPlain429IsNotFunds(t *testing.T) {
	treatAsRemote(t)
	up := fakeUpstream(t, 429, `{"error":{"message":"Rate limit reached for requests","type":"requests"}}`)
	h, _ := refusalRig(t, map[string]config.Model{"fw": modelTo(t, up.URL, "chat")}, nil)
	rec := post(h, "fw")
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "60" ||
		strings.Contains(rec.Body.String(), "upstream_funds_exhausted") {
		t.Fatalf("status %d Retry-After %q body %s, want the unchanged 429", rec.Code, rec.Header().Get("Retry-After"), rec.Body)
	}
}

// With another candidate left, the funds refusal spills and the caller is served.
func TestFundsRefusalSpillsToNextCandidate(t *testing.T) {
	treatAsRemote(t)
	broke := fakeUpstream(t, 402, `{"error":"out of credits"}`)
	ok := fakeUpstream(t, 200, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	a, b := modelTo(t, broke.URL, "chat"), modelTo(t, ok.URL, "chat")
	a.Quality, b.Quality = 1, 1
	h, st := refusalRig(t, map[string]config.Model{"a": a, "b": b},
		map[string]config.Lane{"chat": {Members: []config.LaneMember{{Model: "a"}, {Model: "b"}}}})
	rec := post(h, "chat")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200 from the second candidate", rec.Code, rec.Body)
	}
	rows, _ := st.RecentActivity(store.Window{}, 10, "", "", "")
	var refused bool
	for _, r := range rows {
		if r.Served == "a" && r.Status == 402 && strings.Contains(r.Error, "out of funds") && strings.Contains(r.Error, "tried the next model") {
			refused = true
		}
	}
	if !refused {
		t.Fatalf("rows = %+v, want a 402 funds row for a", rows)
	}
}
