package api

import "testing"

func TestVerifiedStore_RecordAndFor(t *testing.T) {
	s := NewVerifiedStore()
	s.Record("m", Verdict{Modality: "image", RunMode: "warm", Verified: true})
	got := s.For("m")
	if len(got) != 1 || !got[0].Verified {
		t.Fatalf("For = %+v", got)
	}
	if got[0].At == 0 {
		t.Error("At should default to now so a verdict is never undatable")
	}
	// Same (modality, runMode) replaces — the LATEST verdict is the truth.
	s.Record("m", Verdict{Modality: "image", RunMode: "warm", Verified: false})
	if got := s.For("m"); len(got) != 1 || got[0].Verified {
		t.Errorf("re-record should replace in place: %+v", got)
	}
	// Different runMode is a DIFFERENT verdict, not a replacement — that
	// separation is the entire point.
	s.Record("m", Verdict{Modality: "image", RunMode: "cold", Verified: false})
	if got := s.For("m"); len(got) != 2 {
		t.Errorf("cold and warm must coexist: %+v", got)
	}
}

// The signature this whole path exists to surface: a modality that works warm
// and fails cold. A store that could not express the split would report the
// warm verdict alone and call the model verified.
func TestVerifiedStore_Disagreements(t *testing.T) {
	s := NewVerifiedStore()
	s.Record("bonsai", Verdict{Modality: "image", RunMode: "warm", Verified: true})
	s.Record("bonsai", Verdict{Modality: "image", RunMode: "cold", Verified: false})
	if d := s.Disagreements("bonsai"); len(d) != 2 {
		t.Errorf("cold/warm split not reported as a disagreement: %+v", d)
	}

	// Agreement is not a disagreement.
	s.Record("good", Verdict{Modality: "image", RunMode: "warm", Verified: true})
	s.Record("good", Verdict{Modality: "image", RunMode: "cold", Verified: true})
	if d := s.Disagreements("good"); len(d) != 0 {
		t.Errorf("consistent verdicts must not be flagged: %+v", d)
	}

	// A single verdict cannot disagree with anything — importantly it must NOT
	// be reported as a problem just for being alone.
	s.Record("lonely", Verdict{Modality: "image", RunMode: "warm", Verified: true})
	if d := s.Disagreements("lonely"); len(d) != 0 {
		t.Errorf("a single verdict is not a disagreement: %+v", d)
	}
}

// A nil store is valid (no bench has ever run) and every reader must tolerate it.
func TestVerifiedStore_NilSafe(t *testing.T) {
	var s *VerifiedStore
	s.Record("m", Verdict{Modality: "image"})
	if got := s.For("m"); got != nil {
		t.Errorf("nil store should read empty, got %+v", got)
	}
	if got := s.Disagreements("m"); got != nil {
		t.Errorf("nil store disagreements should be empty, got %+v", got)
	}
}
