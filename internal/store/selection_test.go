package store

import (
	"context"
	"testing"
)

func selectionStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestSelectionRoundTrip: upstream, lanes and quality survive. Upstream matters
// most — it is the only record of what to put on the wire, and ServedName is
// lossy, so losing it strands the model.
func TestSelectionRoundTrip(t *testing.T) {
	s := selectionStore(t)
	in := ModelSelection{
		Provider: "openrouter", Credential: "work", Model: "openrouter-x",
		Upstream: "vendor/x:free", Quality: 4.5, Note: "benched",
		Lanes: []LaneRef{{Lane: "chat", Order: 20}, {Lane: "free", Order: 5}},
	}
	if err := s.SaveSelection(in); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadSelections()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 selection, got %d", len(got))
	}
	a := got[0]
	if a.Upstream != "vendor/x:free" || a.Quality != 4.5 || a.Note != "benched" {
		t.Errorf("round trip lost fields: %+v", a)
	}
	if len(a.Lanes) != 2 || a.Lanes[0].Lane != "chat" || a.Lanes[0].Order != 20 {
		t.Errorf("lane placement lost: %+v", a.Lanes)
	}
	if a.At.IsZero() {
		t.Error("At not stamped")
	}
}

// TestSelectionIsPerCredential: directories differ by key, so the same upstream
// id can be wanted on one account and not another.
func TestSelectionIsPerCredential(t *testing.T) {
	s := selectionStore(t)
	for _, a := range []ModelSelection{
		{Provider: "openrouter", Credential: "freekey", Model: "m", Upstream: "v/m:free"},
		{Provider: "openrouter", Credential: "paidkey", Model: "m", Upstream: "v/m"},
	} {
		if err := s.SaveSelection(a); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := s.LoadSelections()
	if len(got) != 2 {
		t.Fatalf("per-credential selections collapsed into %d row(s)", len(got))
	}
	up := map[string]string{}
	for _, a := range got {
		up[a.Credential] = a.Upstream
	}
	if up["freekey"] != "v/m:free" || up["paidkey"] != "v/m" {
		t.Errorf("selections crossed accounts: %+v", up)
	}
}

// TestReassignMovesRatherThanDuplicates: assigning an already-assigned model is
// how a lane change is expressed, so it must update in place.
func TestReassignMovesRatherThanDuplicates(t *testing.T) {
	s := selectionStore(t)
	base := ModelSelection{Provider: "p", Credential: "c", Model: "m", Upstream: "u",
		Lanes: []LaneRef{{Lane: "chat", Order: 10}}}
	_ = s.SaveSelection(base)
	base.Lanes = []LaneRef{{Lane: "batch", Order: 99}}
	if err := s.SaveSelection(base); err != nil {
		t.Fatal(err)
	}
	got, _ := s.LoadSelections()
	if len(got) != 1 {
		t.Fatalf("want one row, got %d", len(got))
	}
	if len(got[0].Lanes) != 1 || got[0].Lanes[0].Lane != "batch" {
		t.Errorf("re-assign did not move it: %+v", got[0].Lanes)
	}
}

// TestReassignKeepsUpstreamWhenOmitted: moving a model between lanes comes from
// a form with no reason to echo the provider's id back, and clearing it would
// strand the model with nothing to address upstream.
func TestReassignKeepsUpstreamWhenOmitted(t *testing.T) {
	s := selectionStore(t)
	_ = s.SaveSelection(ModelSelection{Provider: "p", Credential: "c", Model: "m", Upstream: "vendor/real"})
	_ = s.SaveSelection(ModelSelection{Provider: "p", Credential: "c", Model: "m",
		Lanes: []LaneRef{{Lane: "chat", Order: 1}}})
	got, _ := s.LoadSelections()
	if len(got) != 1 || got[0].Upstream != "vendor/real" {
		t.Errorf("upstream lost on re-placement: %+v", got)
	}
}

// TestUnassignRemovesTheRow. Presence is the whole predicate — there is no
// "rejected" state to fall back to, and that is the point.
func TestUnassignRemovesTheRow(t *testing.T) {
	s := selectionStore(t)
	_ = s.SaveSelection(ModelSelection{Provider: "p", Credential: "c", Model: "junk", Upstream: "u"})
	if err := s.DeleteSelection("p", "c", "junk"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.LoadSelections()
	if len(got) != 0 {
		t.Errorf("unassign left rows behind: %+v", got)
	}
}

// TestApprovalsMigrateToSelections: an operator upgrading from the approval
// queue keeps what they said YES to, and nothing else. A pending row was a
// question nobody answered and a rejected row was a no; neither is a thing to
// serve, and with the gate gone a rejection holds nothing back anyway.
func TestApprovalsMigrateToSelections(t *testing.T) {
	dir := t.TempDir() + "/legacy.db"
	// Stand up the OLD table and populate it, then let Open() migrate.
	s, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`
CREATE TABLE model_approval (
  provider TEXT NOT NULL, credential TEXT NOT NULL, model TEXT NOT NULL,
  state TEXT NOT NULL, lanes TEXT NOT NULL DEFAULT '', quality REAL NOT NULL DEFAULT 0,
  note TEXT NOT NULL DEFAULT '', at INTEGER NOT NULL DEFAULT 0,
  upstream TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (provider, credential, model))`); err != nil {
		t.Fatal(err)
	}
	for _, row := range [][]any{
		{"p", "c", "yes", "approved", `[{"lane":"chat","order":7}]`, 4.0, "", int64(1), "vendor/yes"},
		{"p", "c", "no", "rejected", "", 0.0, "", int64(1), "vendor/no"},
		{"p", "c", "dunno", "pending", "", 0.0, "", int64(1), "vendor/dunno"},
	} {
		if _, err := s.db.Exec(`INSERT INTO model_approval
(provider,credential,model,state,lanes,quality,note,at,upstream) VALUES (?,?,?,?,?,?,?,?,?)`, row...); err != nil {
			t.Fatal(err)
		}
	}
	_ = s.Close()

	s2, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatalf("reopen (migration) failed: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	got, err := s2.LoadSelections()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want only the approved row carried over, got %+v", got)
	}
	if got[0].Model != "yes" || got[0].Upstream != "vendor/yes" {
		t.Errorf("wrong row migrated: %+v", got[0])
	}
	if len(got[0].Lanes) != 1 || got[0].Lanes[0].Order != 7 {
		t.Errorf("placement lost in migration: %+v", got[0].Lanes)
	}
	// And opening AGAIN must not fail now that the source table is gone —
	// the migration list re-runs on every open.
	s3, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatalf("second reopen after the source table was dropped: %v", err)
	}
	_ = s3.Close()
}
