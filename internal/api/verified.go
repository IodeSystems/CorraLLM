package api

import (
	"sync"
	"time"
)

// Verdict is one observed capability result from a capability probe.
type Verdict struct {
	Modality string `json:"modality"`
	// RunMode matters: a modality can work warm and fail cold. A verdict
	// without its residency state cannot be interpreted, and reporting the warm
	// verdict alone is how a broken cold path stays invisible.
	RunMode  string `json:"runMode,omitempty"`
	Verified bool   `json:"verified"`
	Probe    string `json:"probe,omitempty"`
	Detail   string `json:"detail,omitempty"`
	At       int64  `json:"at"`
}

// VerifiedStore holds the most recent capability verdict per
// (model, modality, runMode).
//
// Deliberately in-memory and NOT persisted: a verdict describes what a specific
// build of a backend did at a specific moment, and a stale "verified" surviving
// a model swap or a llama.cpp upgrade would be worse than no verdict at all —
// it would assert, with authority, something nobody had checked since. Losing
// verdicts on restart is the safe direction; re-running the probes is cheap.
type VerifiedStore struct {
	mu sync.RWMutex
	// model -> "modality/runMode" -> verdict
	data map[string]map[string]Verdict
}

// NewVerifiedStore builds an empty store.
func NewVerifiedStore() *VerifiedStore {
	return &VerifiedStore{data: map[string]map[string]Verdict{}}
}

func verdictKey(modality, runMode string) string { return modality + "/" + runMode }

// Record stores v, replacing any prior verdict for the same
// (model, modality, runMode).
func (s *VerifiedStore) Record(model string, v Verdict) {
	if s == nil {
		return
	}
	if v.At == 0 {
		v.At = time.Now().Unix()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data[model] == nil {
		s.data[model] = map[string]Verdict{}
	}
	s.data[model][verdictKey(v.Modality, v.RunMode)] = v
}

// For returns every verdict recorded for model.
func (s *VerifiedStore) For(model string) []Verdict {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Verdict, 0, len(s.data[model]))
	for _, v := range s.data[model] {
		out = append(out, v)
	}
	return out
}

// Disagreements returns verdicts where a modality was verified in one residency
// state and NOT in another — the cold/warm split that a single-state probe can
// never surface, and the exact signature of the bug this whole path exists for.
func (s *VerifiedStore) Disagreements(model string) []Verdict {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	byModality := map[string][]Verdict{}
	for _, v := range s.data[model] {
		byModality[v.Modality] = append(byModality[v.Modality], v)
	}
	var out []Verdict
	for _, vs := range byModality {
		if len(vs) < 2 {
			continue
		}
		anyTrue, anyFalse := false, false
		for _, v := range vs {
			if v.Verified {
				anyTrue = true
			} else {
				anyFalse = true
			}
		}
		if anyTrue && anyFalse {
			out = append(out, vs...)
		}
	}
	return out
}
