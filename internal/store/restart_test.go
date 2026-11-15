package store

import (
	"context"
	"testing"
	"time"
)

func TestBackendRestartLifecycle(t *testing.T) {
	st, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	now := time.Now()
	stale, _ := st.AddBackendRestart("swift", "old", 0, now.Add(-time.Hour))
	a, _ := st.AddBackendRestart("swift", "new build", 2*time.Minute, now)
	b, _ := st.AddBackendRestart("embed", "", 0, now)

	got, err := st.ClaimBackendRestarts(now.Add(-time.Minute), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != a || got[1].ID != b || got[0].Drain != 2*time.Minute || got[0].Reason != "new build" {
		t.Fatalf("claimed %+v", got)
	}
	if r, _ := st.GetBackendRestart(stale); r.State != RestartFailed {
		t.Errorf("stale request %q, want failed", r.State)
	}
	if again, _ := st.ClaimBackendRestarts(now.Add(-time.Minute), now); len(again) != 0 {
		t.Errorf("claimed twice: %+v", again)
	}
	if ok, _ := st.AbandonBackendRestart(a, "gave up", now); ok {
		t.Error("abandoned a claimed request")
	}
	_ = st.UpdateBackendRestart(a, RestartDone, "pid 1 -> 2", now)
	if r, _ := st.GetBackendRestart(a); !r.Finished() || r.Detail != "pid 1 -> 2" {
		t.Errorf("after update %+v", r)
	}
	c, _ := st.AddBackendRestart("swift", "", 0, now)
	if ok, _ := st.AbandonBackendRestart(c, "no daemon", now); !ok {
		t.Error("could not abandon a pending request")
	}
}
