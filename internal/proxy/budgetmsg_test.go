package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
)

// A refusal names whose budget refused, its spend and when it has room — not
// only "budget exhausted, retry after N seconds".
func TestRefusalNamesTheBudget(t *testing.T) {
	k := newChildKeyRig(t, config.LimitSet{{Req: 1, Per: "calendar-day", Hard: true}})
	if c := k.chat("parent", "mock"); c != http.StatusOK {
		t.Fatalf("first: %d", c)
	}
	rec := k.do(http.MethodPost, "/v1/chat/completions", "parent", `{"model":"mock"}`)
	today, tomorrow := time.Now().UTC().Format("2006-01-02"), time.Now().UTC().AddDate(0, 0, 1).Format("2006-01-02")
	var body struct {
		Error struct {
			Message string       `json:"message"`
			Budgets []BudgetNote `json:"budgets"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusPaymentRequired || len(body.Error.Budgets) != 1 ||
		!strings.Contains(body.Error.Message, "key pa…'s budget on everything: budget of 1 requests created on "+today+", expires on "+tomorrow+" 00:00 UTC; 1 requests spent") ||
		body.Error.Budgets[0].RoomAt == "" {
		t.Errorf("%d %s", rec.Code, rec.Body.String())
	}

	m := k.cfg.Models["other"]
	m.FreeTier = &config.FreeTier{Limits: config.LimitSet{{Req: 1, Per: "calendar-day", Hard: true}}}
	k.cfg.Models["other"] = m
	k.mount()
	if c := k.chat("peer", "other"); c != http.StatusOK {
		t.Fatalf("other first: %d", c)
	}
	rec = k.do(http.MethodPost, "/v1/chat/completions", "peer", `{"model":"other"}`)
	if rec.Code != http.StatusPaymentRequired || !strings.Contains(rec.Body.String(), "the model-wide limit on other (every key)") ||
		!strings.Contains(rec.Body.String(), "not your key's own limit") {
		t.Errorf("model cap: %d %s", rec.Code, rec.Body.String())
	}
}

func TestHumanWait(t *testing.T) {
	for d, want := range map[time.Duration]string{30 * time.Second: "30 s", 6 * time.Minute: "6 min", 19*time.Hour + 47*time.Minute: "19 h 47 min", 72 * time.Hour: "3 days"} {
		if got := humanWait(d); got != want {
			t.Errorf("%v = %q, want %q", d, got, want)
		}
	}
}

// A caller with no key entry spends no declared account: filters and budgets
// live on keys, and a stranger has none.
func TestStrangerSpendsNoAccount(t *testing.T) {
	k := newChildKeyRig(t, nil)
	paid := config.Candidate{Name: "m", Model: config.Model{ProviderName: "fw"}, Credential: &config.Credential{Name: "main"}}
	if k.permits(k.px.resolveCaller("someone-on-the-lan"), paid) {
		t.Error("a stranger may forward with a declared credential")
	}
	if !k.permits(k.px.resolveCaller("parent"), paid) {
		t.Error("a configured key with no models filter lost every account")
	}
	open := config.Candidate{Name: "local", Model: config.Model{}}
	if !k.permits(k.px.resolveCaller("someone-on-the-lan"), open) {
		t.Error("a stranger lost a backend with no credential")
	}
}

func TestRefusalStatusByWindow(t *testing.T) {
	for _, c := range []struct {
		window string
		want   int
		retry  bool
	}{
		{"usd/hour", http.StatusTooManyRequests, true},
		{"req/minute", http.StatusTooManyRequests, true},
		{"usd/calendar-day", http.StatusPaymentRequired, false},
		{"usd/calendar-month", http.StatusPaymentRequired, false},
		{"req/total", http.StatusPaymentRequired, false},
	} {
		rec := httptest.NewRecorder()
		got := refusalStatus([]BudgetNote{{Window: c.window}}, 30, rec)
		if got != c.want || (rec.Header().Get("Retry-After") != "") != c.retry {
			t.Errorf("%s: %d, Retry-After %q; want %d, retry %v", c.window, got, rec.Header().Get("Retry-After"), c.want, c.retry)
		}
	}
	// A calendar window beside a rolling one: out until the reset either way.
	if got := refusalStatus([]BudgetNote{{Window: "usd/hour"}, {Window: "usd/calendar-day"}}, 30, httptest.NewRecorder()); got != http.StatusPaymentRequired {
		t.Errorf("mixed: %d, want 402", got)
	}
}

func TestCapabilitiesStateTheErrorContract(t *testing.T) {
	raw, _ := json.Marshal(errorContract)
	for _, want := range []string{`"status":402`, `"status":429`, "do not retry", "allocation_spent"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("error contract lacks %q: %s", want, raw)
		}
	}
}
