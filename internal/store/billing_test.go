package store

import (
	"context"
	"testing"
)

func TestProviderBillingRoundTrip(t *testing.T) {
	st, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	const h = int64(3600000)
	old := BillingRow{Provider: "fw", Credential: "default", Upstream: "m", HourMS: 10 * h, BilledUSD: 1, RecordedUSD: 1}
	a := BillingRow{Provider: "fw", Credential: "default", Upstream: "m", Served: "fw-m", HourMS: 50 * h, BilledUSD: 2, RecordedUSD: 0.5, Incomplete: true}
	if err := st.ReplaceBilling("fw", "default", 0, 1000, []BillingRow{old, a}, true); err != nil {
		t.Fatal(err)
	}
	// A second fetch replaces rows from `since` on and keeps older hours.
	a.BilledUSD = 2.5
	if err := st.ReplaceBilling("fw", "default", 40*h, 2000, []BillingRow{a}, false); err != nil {
		t.Fatal(err)
	}
	rows, err := st.BillingRows(0)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows = %+v, %v", rows, err)
	}
	if rows[0].HourMS != 10*h || rows[1].BilledUSD != 2.5 || rows[1].Served != "fw-m" || !rows[1].Incomplete {
		t.Errorf("rows = %+v", rows)
	}
	if rows, _ := st.BillingRows(20 * h); len(rows) != 1 {
		t.Errorf("since filter = %+v", rows)
	}

	// Another credential is independent.
	if err := st.ReplaceBilling("fw", "other", 0, 3000, nil, false); err != nil {
		t.Fatal(err)
	}
	if rows, _ := st.BillingRows(0); len(rows) != 2 {
		t.Errorf("another credential's replace changed rows: %+v", rows)
	}

	// An error keeps rows and the last success time.
	if err := st.RecordBillingError("fw", "default", 4000, "HTTP 500"); err != nil {
		t.Fatal(err)
	}
	fs, err := st.BillingFetches()
	if err != nil || len(fs) != 2 {
		t.Fatalf("fetches = %+v, %v", fs, err)
	}
	f := fs[0]
	if f.Credential != "default" || f.Error != "HTTP 500" || f.FetchedMS != 4000 || f.OKMS != 2000 || f.Incomplete {
		t.Errorf("fetch = %+v", f)
	}
	if rows, _ := st.BillingRows(0); len(rows) != 2 {
		t.Errorf("a failed fetch changed rows: %+v", rows)
	}
	// Success clears the error.
	if err := st.ReplaceBilling("fw", "default", 40*h, 5000, []BillingRow{a}, false); err != nil {
		t.Fatal(err)
	}
	if fs, _ := st.BillingFetches(); fs[0].Error != "" || fs[0].OKMS != 5000 {
		t.Errorf("fetch after success = %+v", fs[0])
	}
}

func TestRecordedCostByHour(t *testing.T) {
	st, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	const h = int64(3600000)
	ins := func(ts int64, served string, cost float64) {
		if _, err := st.DB().Exec(`INSERT INTO activity (ts, served, path, status, cost_usd) VALUES (?,?,?,?,?)`,
			ts, served, "/v1/x", 200, cost); err != nil {
			t.Fatal(err)
		}
	}
	ins(5*h+10, "a", 0.1)
	ins(5*h+20, "a", 0.2)
	ins(6*h, "a", 0.4)
	ins(5*h, "b", 1)
	ins(5*h, "c", 9) // not asked for
	ins(9*h, "a", 5) // outside the window
	got, err := st.RecordedCostByHour([]string{"a", "b"}, 5*h, 8*h)
	if err != nil {
		t.Fatal(err)
	}
	if v := got["a"][5*h]; v < 0.2999 || v > 0.3001 {
		t.Errorf("a@5 = %v, want 0.3", got["a"][5*h])
	}
	if got["a"][6*h] != 0.4 || got["b"][5*h] != 1 || len(got["c"]) != 0 || len(got["a"]) != 2 {
		t.Errorf("got = %v", got)
	}
	if got, _ := st.RecordedCostByHour(nil, 0, 1); len(got) != 0 {
		t.Error("no models: want empty")
	}
}
