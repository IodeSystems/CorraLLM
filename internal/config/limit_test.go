package config

import (
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestLimitSetListForm(t *testing.T) {
	var ls LimitSet
	in := `
- {req: 20, per: minute}
- {req: 1000, per: day}
- {usd: 200, per: month}
`
	if err := yaml.Unmarshal([]byte(in), &ls); err != nil {
		t.Fatal(err)
	}
	if len(ls) != 3 {
		t.Fatalf("got %d limits, want 3: %+v", len(ls), ls)
	}
	if err := ls.Validate("test"); err != nil {
		t.Fatal(err)
	}
	byLabel := map[string]Limit{}
	for _, l := range ls {
		byLabel[l.Label()] = l
	}
	for _, want := range []struct {
		label string
		amt   float64
		win   time.Duration
	}{
		{"req/minute", 20, time.Minute},
		{"req/day", 1000, 24 * time.Hour},
		{"usd/month", 200, 30 * 24 * time.Hour},
	} {
		l, ok := byLabel[want.label]
		if !ok {
			t.Fatalf("missing %s in %+v", want.label, byLabel)
		}
		if l.Amount() != want.amt {
			t.Errorf("%s amount = %v, want %v", want.label, l.Amount(), want.amt)
		}
		w, _ := l.Window()
		if w != want.win {
			t.Errorf("%s window = %v, want %v", want.label, w, want.win)
		}
	}
}

// TestLimitSetTwoWindowsOneDimension is the whole reason for a list: a map
// keyed by dimension cannot hold "20/minute AND 1000/day".
func TestLimitSetTwoWindowsOneDimension(t *testing.T) {
	var ls LimitSet
	if err := yaml.Unmarshal([]byte("- {req: 20, per: minute}\n- {req: 1000, per: day}\n"), &ls); err != nil {
		t.Fatal(err)
	}
	if len(ls.ForDimension(DimRequests)) != 2 {
		t.Errorf("want both request windows kept, got %+v", ls)
	}
	if ls[0].Label() == ls[1].Label() {
		t.Error("two windows on one dimension must have distinct labels (they key separate counters)")
	}
}

// TestLimitSetLegacyRPMRPD: freeTier's existing shape must keep parsing — it is
// live in the running config today.
func TestLimitSetLegacyRPMRPD(t *testing.T) {
	var ls LimitSet
	if err := yaml.Unmarshal([]byte("{rpm: 20, rpd: 1000}"), &ls); err != nil {
		t.Fatal(err)
	}
	if len(ls) != 2 {
		t.Fatalf("got %+v, want 2 limits", ls)
	}
	if err := ls.Validate("legacy"); err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, l := range ls {
		got[l.Label()] = l.Amount()
	}
	if got["req/minute"] != 20 || got["req/day"] != 1000 {
		t.Errorf("legacy rpm/rpd = %+v, want req/minute 20 and req/day 1000", got)
	}
}

// TestLimitSetLegacyRateSpecs: the group/stage "cost: $5/hr" mini-language.
func TestLimitSetLegacyRateSpecs(t *testing.T) {
	var ls LimitSet
	if err := yaml.Unmarshal([]byte(`{cost: "$5/hr", dwell: "600s/min"}`), &ls); err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, l := range ls {
		got[l.Label()] = l.Amount()
	}
	if got["usd/hour"] != 5 {
		t.Errorf("cost $5/hr -> %+v, want usd/hour 5", got)
	}
	if got["sec/minute"] != 600 {
		t.Errorf("dwell 600s/min -> %+v, want sec/minute 600", got)
	}
}

// TestLimitValidationRejectsAmbiguous: an entry naming no dimension, or two,
// has no meaning — catching it at config load beats silently counting nothing.
func TestLimitValidationRejectsAmbiguous(t *testing.T) {
	for _, tc := range []struct {
		name string
		l    Limit
	}{
		{"no dimension", Limit{Per: "hour"}},
		{"two dimensions", Limit{Req: 5, USD: 5, Per: "hour"}},
		{"missing per", Limit{Req: 5}},
		{"unknown window", Limit{Req: 5, Per: "fortnight"}},
		{"non-positive", Limit{Req: -1, Per: "hour"}},
	} {
		if err := tc.l.Validate(); err == nil {
			t.Errorf("%s: want an error, got nil", tc.name)
		}
	}
}

func TestRenewsParsesAndValidates(t *testing.T) {
	l, err := ParseLimit("$200/calendar-month,renews=15,hard")
	if err != nil || l.Renews != 15 || !l.Hard || l.USD != 200 {
		t.Fatalf("limit = %+v, err %v", l, err)
	}
	if got := l.Compact(); got != "$200/calendar-month,renews=15,hard" {
		t.Errorf("compact = %q", got)
	}
	for _, bad := range []Limit{
		{USD: 1, Per: "calendar-month", Renews: 29},
		{USD: 1, Per: "calendar-day", Renews: 15},
	} {
		if bad.Validate() == nil {
			t.Errorf("%+v: want an error", bad)
		}
	}
}

func TestPacedAllowance(t *testing.T) {
	// 11-04 12:00, a month renewing on the 15th: 11 days left, today included.
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	if got, want := PacedAllowance(200, 6.85, 0, 15, now), (200-6.85)/11; got != want {
		t.Errorf("allowance = %v, want %v", got, want)
	}
	// Spend today raises both levels together, so the allowance holds all day.
	if got, want := PacedAllowance(200, 16.85, 10, 15, now), (200-6.85)/11; got != want {
		t.Errorf("allowance after spend = %v, want %v", got, want)
	}
	// The last day gets all that is left.
	last := time.Date(2026, 11, 14, 23, 0, 0, 0, time.UTC)
	if got := PacedAllowance(200, 150, 0, 15, last); got != 50 {
		t.Errorf("last day = %v, want 50", got)
	}
	// A spent month leaves nothing, not a negative.
	if got := PacedAllowance(200, 210, 0, 15, now); got != 0 {
		t.Errorf("overspent = %v, want 0", got)
	}
}

func TestPaceParseAndValidate(t *testing.T) {
	l, err := ParseLimit("pace/calendar-day,hard")
	if err != nil {
		t.Fatal(err)
	}
	if !l.Pace || !l.Hard || l.Dimension() != DimUSD || l.Compact() != "pace/calendar-day,hard" {
		t.Fatalf("parsed = %+v (%s)", l, l.Compact())
	}
	if _, err := ParseLimit("pace/calendar-month"); err == nil {
		t.Error("a paced month must be refused")
	}
	month := Limit{USD: 200, Per: "calendar-month", Renews: 15, Hard: true}
	if err := (LimitSet{month, l}).Validate("p"); err != nil {
		t.Errorf("pace beside a month: %v", err)
	}
	if err := (LimitSet{l}).Validate("p"); err == nil {
		t.Error("pace with no month beside it must be refused")
	}
	if err := (LimitSet{month, l}).ValidateKey(); err != nil {
		t.Errorf("pace on a key: %v", err)
	}
	if err := (LimitSet{{USD: 5, Pace: true, Per: "calendar-day"}, month}).Validate("p"); err == nil {
		t.Error("a paced limit with an amount must be refused")
	}
}
