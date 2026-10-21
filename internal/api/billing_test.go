package api

import (
	"context"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/store"
)

func TestProviderBillingSumsPerModelAndDay(t *testing.T) {
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	now := time.Now().UTC()
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	rows := []store.BillingRow{
		{Provider: "fw", Credential: "default", Upstream: "accounts/x/models/a", Served: "fw-a", HourMS: month.UnixMilli(), BilledUSD: 1, RecordedUSD: 0.25},
		{Provider: "fw", Credential: "default", Upstream: "accounts/x/models/a", Served: "fw-a", HourMS: today.UnixMilli(), BilledUSD: 2, RecordedUSD: 1},
		{Provider: "fw", Credential: "default", Upstream: "accounts/x/models/b", HourMS: today.UnixMilli(), BilledUSD: 0.5},
	}
	if err := st.ReplaceBilling("fw", "default", 0, now.UnixMilli(), rows, true); err != nil {
		t.Fatal(err)
	}
	out, err := (&Handlers{Store: st}).ProviderBilling(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ps := out.Body.Providers
	if len(ps) != 1 {
		t.Fatalf("providers = %+v", ps)
	}
	p := ps[0]
	if p.BilledMonthUSD != 3.5 || p.RecordedMonthUSD != 1.25 || p.BilledTodayUSD != 2.5 || p.RecordedTodayUSD != 1 || !p.Settling {
		t.Errorf("totals = %+v", p)
	}
	if len(p.Models) != 2 || p.Models[0].Upstream != "accounts/x/models/a" || p.Models[0].BilledMonthUSD != 3 ||
		p.Models[0].BilledTodayUSD != 2 || p.Models[0].Served != "fw-a" {
		t.Errorf("models = %+v", p.Models)
	}
	// The month's first day and today are the same day only on the 1st.
	if want := 2; today.Equal(month) {
		want = 1
		if len(p.Days) != want {
			t.Errorf("days = %+v", p.Days)
		}
	} else if len(p.Days) != want || p.Days[0].Day != today.Format("2006-01-02") {
		t.Errorf("days = %+v, want newest first", p.Days)
	}
}
