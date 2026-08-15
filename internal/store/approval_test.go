package store

import (
	"context"
	"testing"
)

func approvalStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestApprovalRoundTrip: state, lanes and quality survive, because the lane
// placement chosen at approval is the whole point of recording it.
func TestApprovalRoundTrip(t *testing.T) {
	s := approvalStore(t)
	in := ModelApproval{
		Provider: "openrouter", Credential: "work", Model: "openrouter-x",
		State: ApprovalApproved, Quality: 4.5, Note: "benched",
		Lanes: []LaneRef{{Lane: "chat", Order: 20}, {Lane: "free", Order: 5}},
	}
	if err := s.SaveApproval(in); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadApprovals()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 approval, got %d", len(got))
	}
	a := got[0]
	if a.State != ApprovalApproved || a.Quality != 4.5 || a.Note != "benched" {
		t.Errorf("round trip lost fields: %+v", a)
	}
	if len(a.Lanes) != 2 || a.Lanes[0].Lane != "chat" || a.Lanes[0].Order != 20 {
		t.Errorf("lane placement lost: %+v", a.Lanes)
	}
	if a.At.IsZero() {
		t.Error("At not stamped")
	}
}

// TestApprovalIsPerCredential: the same upstream id can be wanted on one
// account and refused on another — a paid key's model is a spending decision
// the free key's is not.
func TestApprovalIsPerCredential(t *testing.T) {
	s := approvalStore(t)
	for _, a := range []ModelApproval{
		{Provider: "openrouter", Credential: "freekey", Model: "m", State: ApprovalApproved},
		{Provider: "openrouter", Credential: "paidkey", Model: "m", State: ApprovalRejected},
	} {
		if err := s.SaveApproval(a); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := s.LoadApprovals()
	if len(got) != 2 {
		t.Fatalf("per-credential decisions collapsed into %d row(s)", len(got))
	}
	states := map[string]string{}
	for _, a := range got {
		states[a.Credential] = a.State
	}
	if states["freekey"] != ApprovalApproved || states["paidkey"] != ApprovalRejected {
		t.Errorf("decisions crossed accounts: %+v", states)
	}
}

// TestApprovalReplacesInPlace: re-deciding updates rather than duplicating.
func TestApprovalReplacesInPlace(t *testing.T) {
	s := approvalStore(t)
	base := ModelApproval{Provider: "p", Credential: "c", Model: "m", State: ApprovalPending}
	_ = s.SaveApproval(base)
	base.State = ApprovalRejected
	if err := s.SaveApproval(base); err != nil {
		t.Fatal(err)
	}
	got, _ := s.LoadApprovals()
	if len(got) != 1 || got[0].State != ApprovalRejected {
		t.Errorf("want one row in the rejected state, got %+v", got)
	}
}

// TestRejectionSurvivesReload is why this is persisted at all: a rejection that
// vanished on restart would put the model back in the queue every refresh and
// ask the operator the same question forever.
func TestRejectionSurvivesReload(t *testing.T) {
	s := approvalStore(t)
	_ = s.SaveApproval(ModelApproval{Provider: "p", Credential: "c", Model: "junk", State: ApprovalRejected})
	got, _ := s.LoadApprovals()
	if len(got) != 1 || got[0].State != ApprovalRejected {
		t.Fatalf("rejection not durable: %+v", got)
	}
	if err := s.DeleteApproval("p", "c", "junk"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.LoadApprovals()
	if len(got) != 0 {
		t.Errorf("delete should return the model to pending, got %+v", got)
	}
}
