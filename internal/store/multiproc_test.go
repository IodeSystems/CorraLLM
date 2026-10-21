package store

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// helperEnv names the database a re-executed test binary writes to. Set only
// in the child started by TestMultiProcess_ConcurrentWriters.
const helperEnv = "CORRALLM_STORE_HELPER_DB"

const writesPerProcess = 1000

// writeLoad is what one daemon does to the database per request during a
// blue-green overlap: a marker, a completed row that deletes it in its own
// transaction, and a spend charge.
func writeLoad(st *Store, who string) error {
	for i := 0; i < writesPerProcess; i++ {
		now := time.Now().UnixMilli()
		id, err := st.BeginInflight(Activity{TS: now, Served: who, Path: "/v1/chat/completions"})
		if err != nil {
			return fmt.Errorf("%s marker %d: %w", who, i, err)
		}
		if err := st.InsertActivity(Activity{TS: now, Served: who, Path: "/v1/chat/completions",
			Status: 200, InflightID: id, ReqBody: "x"}); err != nil {
			return fmt.Errorf("%s row %d: %w", who, i, err)
		}
		if _, err := st.AddQuotaCounter("fw", "usd/calendar-month", 0.01, 50, 0, 1, now); err != nil {
			return fmt.Errorf("%s charge %d: %w", who, i, err)
		}
	}
	return nil
}

// TestHelperProcessWriter is the child half of the two-process test. It does
// nothing unless started by it.
func TestHelperProcessWriter(t *testing.T) {
	path := os.Getenv(helperEnv)
	if path == "" {
		t.Skip("child half of TestMultiProcess_ConcurrentWriters")
	}
	// Opening applies the schema and migrations, the way a successor's start
	// does while its predecessor is writing.
	st, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	st.SetOwner(os.Getpid(), "child")
	// Tell the parent to start, so the two write loops overlap.
	if err := os.WriteFile(path+".started", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeLoad(st, "child"); err != nil {
		t.Fatal(err)
	}
}

// Two PROCESSES write the same database at once — the old and new daemon in a
// handoff. Every write must land (busy_timeout waits instead of failing with
// SQLITE_BUSY), and the spend counter must end at the sum of both.
func TestMultiProcess_ConcurrentWriters(t *testing.T) {
	if os.Getenv(helperEnv) != "" {
		t.Skip("inside the child")
	}
	path := filepath.Join(t.TempDir(), "corrallm.db")
	st, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	st.SetOwner(os.Getpid(), "parent")

	child := exec.Command(os.Args[0], "-test.run=^TestHelperProcessWriter$", "-test.count=1")
	child.Env = append(os.Environ(), helperEnv+"="+path)
	var wg sync.WaitGroup
	var childOut []byte
	var childErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		childOut, childErr = child.CombinedOutput()
	}()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if _, err := os.Stat(path + ".started"); err == nil {
			break
		}
	}
	if err := writeLoad(st, "parent"); err != nil {
		t.Error(err)
	}
	wg.Wait()
	if childErr != nil {
		t.Fatalf("child: %v\n%s", childErr, childOut)
	}

	var rows, markers int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM activity`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	markers, _ = st.InflightCount()
	if rows != 2*writesPerProcess || markers != 0 {
		t.Errorf("rows %d markers %d; want %d and 0", rows, markers, 2*writesPerProcess)
	}
	var used float64
	if err := st.db.QueryRow(`SELECT used FROM quota_counter WHERE backend='fw'`).Scan(&used); err != nil {
		t.Fatal(err)
	}
	if want := 0.01 * 2 * writesPerProcess; used < want-1e-6 || used > want+1e-6 {
		t.Errorf("spend = %v, want %v: a process's charges were lost or doubled", used, want)
	}
}

// AddQuotaCounter decays like the ledger: a falloff window leaks limit per
// duration before the charge lands, and a calendar window starts over in a
// new period.
func TestAddQuotaCounter_Decays(t *testing.T) {
	st, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	day := int64(24 * time.Hour / time.Millisecond)
	if u, _ := st.AddQuotaCounter("b", "req/day", 600, 1000, day, 0, 0); u != 600 {
		t.Fatalf("first = %v", u)
	}
	// Half a day later 500 has leaked out: 600 → 100, +1.
	if u, _ := st.AddQuotaCounter("b", "req/day", 1, 1000, day, 0, day/2); u < 100.999 || u > 101.001 {
		t.Errorf("after half a day = %v, want 101", u)
	}
	// Calendar: charged in period starting at 1000, then a new period at 5000.
	if u, _ := st.AddQuotaCounter("b", "usd/calendar-day", 2, 5, day, 1000, 1500); u != 2 {
		t.Errorf("calendar first = %v", u)
	}
	if u, _ := st.AddQuotaCounter("b", "usd/calendar-day", 1, 5, day, 1000, 1600); u != 3 {
		t.Errorf("calendar same period = %v, want 3", u)
	}
	if u, _ := st.AddQuotaCounter("b", "usd/calendar-day", 1, 5, day, 5000, 5100); u != 1 {
		t.Errorf("calendar new period = %v, want 1", u)
	}
	if u, _ := st.RaiseQuotaCounter("b", "usd/calendar-day", 4, 5000, 5200); u != 4 {
		t.Errorf("raise = %v, want 4", u)
	}
	if u, _ := st.RaiseQuotaCounter("b", "usd/calendar-day", 2, 5000, 5300); u != 4 {
		t.Errorf("raise below = %v, want 4 (never lowers)", u)
	}
}
