package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

// openTestStore is an in-memory store, matching what the rest of this package's
// tests do inline.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestToolBuildRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	start := time.Now().Add(-3 * time.Minute)

	id, err := s.StartToolBuild(ctx, "llama.cpp", "box1", start)
	if err != nil {
		t.Fatal(err)
	}

	// Visible as running BEFORE it finishes — that is the point of writing at
	// start rather than only at completion.
	rows, err := s.RecentToolBuilds(ctx, "", "", 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
	if rows[0].Status != "running" || !rows[0].FinishedAt.IsZero() {
		t.Errorf("in-flight row = %+v", rows[0])
	}

	fin := start.Add(193 * time.Second)
	if err := s.FinishToolBuild(ctx, id, ToolBuild{
		Status: "ok", FinishedAt: fin, Version: "build 10497", Stamp: "head=abc archs=86;120",
		Log: "configuring\ncompiling\ndone",
	}); err != nil {
		t.Fatal(err)
	}

	rows, _ = s.RecentToolBuilds(ctx, "llama.cpp", "box1", 10)
	if len(rows) != 1 {
		t.Fatalf("rows=%d", len(rows))
	}
	got := rows[0]
	if got.Status != "ok" || got.Version != "build 10497" {
		t.Errorf("finished row = %+v", got)
	}
	if d := got.FinishedAt.Sub(got.StartedAt); d != 193*time.Second {
		t.Errorf("elapsed = %v, want 193s", d)
	}
	// The listing must not carry logs: twenty builds would be megabytes to
	// render a list of dates.
	if got.Log != "" {
		t.Errorf("listing carried a log (%d bytes); fetch it per build", len(got.Log))
	}

	log, err := s.ToolBuildLog(ctx, id)
	if err != nil || !strings.Contains(log, "compiling") {
		t.Errorf("log = %q err=%v", log, err)
	}
}

// A build is a child of the daemon, so a restart kills it. A row left saying
// "running" is a lie that never resolves — and a spinner in the UI for a build
// that died days ago.
func TestInterruptStaleToolBuilds(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if _, err := s.StartToolBuild(ctx, "ninfer", "box1", time.Now()); err != nil {
		t.Fatal(err)
	}
	n, err := s.InterruptStaleToolBuilds(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("settled %d rows, want 1", n)
	}

	rows, _ := s.RecentToolBuilds(ctx, "", "", 10)
	if rows[0].Status != "interrupted" {
		t.Errorf("status = %q, want interrupted", rows[0].Status)
	}
	if rows[0].FinishedAt.IsZero() {
		t.Error("an interrupted build still has no end time, so it reads as running forever")
	}
	if !strings.Contains(rows[0].Error, "restart") {
		t.Errorf("error does not say why: %q", rows[0].Error)
	}

	// Idempotent: a second startup must not re-settle what it already settled.
	if n, _ := s.InterruptStaleToolBuilds(ctx); n != 0 {
		t.Errorf("re-settled %d already-interrupted rows", n)
	}
}

// The tail is what diagnoses a failure — cmake's error is within a few lines of
// the end — so an oversized log must lose its head, not its end.
func TestOversizedLogKeepsTheTail(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	id, _ := s.StartToolBuild(ctx, "llama.cpp", "box1", time.Now())

	big := strings.Repeat("noise\n", maxBuildLogBytes/6+2000) + "FATAL: the actual error"
	if err := s.FinishToolBuild(ctx, id, ToolBuild{
		Status: "failed", FinishedAt: time.Now(), Error: "build failed", Log: big,
	}); err != nil {
		t.Fatal(err)
	}

	log, err := s.ToolBuildLog(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(log, "FATAL: the actual error") {
		t.Error("the end of the log was trimmed away — that is the part worth keeping")
	}
	if !strings.Contains(log, "trimmed") {
		t.Error("truncation is silent; a reader cannot tell the log is partial")
	}
	if len(log) > maxBuildLogBytes+200 {
		t.Errorf("stored %d bytes, cap is %d", len(log), maxBuildLogBytes)
	}
}

func TestPruneKeepsNewest(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	base := time.Now().Add(-time.Hour)
	for i := range 10 {
		if _, err := s.StartToolBuild(ctx, "llama.cpp", "box1", base.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.PruneToolBuilds(ctx, 4); err != nil {
		t.Fatal(err)
	}
	rows, _ := s.RecentToolBuilds(ctx, "", "", 50)
	if len(rows) != 4 {
		t.Fatalf("kept %d rows, want 4", len(rows))
	}
	// Newest first, and the newest are what survived.
	if !rows[0].StartedAt.After(rows[3].StartedAt) {
		t.Error("ordering is not newest-first")
	}
}
