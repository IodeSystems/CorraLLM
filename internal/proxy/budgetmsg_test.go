package proxy

import (
	"encoding/json"
	"net/http"
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
	var body struct {
		Error struct {
			Message string       `json:"message"`
			Budgets []BudgetNote `json:"budgets"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusTooManyRequests || len(body.Error.Budgets) != 1 ||
		!strings.Contains(body.Error.Message, "key parent's own limit: 1 requests of 1 requests spent today (UTC)") ||
		!strings.Contains(body.Error.Message, "UTC)") || body.Error.Budgets[0].RoomAt == "" {
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
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "the model-wide limit on other (every key)") ||
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

// A caller with no key entry spends no declared account, whatever the keys'
// patterns say: patterns live on keys, and "no patterns" means every account.
func TestStrangerSpendsNoAccount(t *testing.T) {
	k := newChildKeyRig(t, nil)
	paid := config.Candidate{Name: "m", Model: config.Model{ProviderName: "fw"}, Credential: &config.Credential{Name: "main"}}
	if v := k.px.credentialVerdict(k.px.resolveCaller("someone-on-the-lan"), paid); v.allowed {
		t.Error("a stranger may forward with a declared credential")
	}
	if v := k.px.credentialVerdict(k.px.resolveCaller("parent"), paid); !v.allowed {
		t.Error("a configured key with no patterns lost every account")
	}
	open := config.Candidate{Name: "local", Model: config.Model{}}
	if v := k.px.credentialVerdict(k.px.resolveCaller("someone-on-the-lan"), open); !v.allowed {
		t.Error("a stranger lost a backend with no credential")
	}
}
