package run

import (
	"context"
	"sort"
	"sync"

	"github.com/iodesystems/agentkit/agent"
)

// memStore is a minimal in-memory agent.Store for a single llm-bench session.
// Crucible drives stages by appending a user Entry then calling Turn, so there
// is no async inbox: ClaimPending always reports zero.
type memStore struct {
	mu      sync.Mutex
	entries []agent.Entry
}

func (s *memStore) ClaimPending(_ context.Context, _ string, _ int64) (int, error) {
	return 0, nil
}

func (s *memStore) Append(_ context.Context, _ string, e agent.Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, e)
	return nil
}

func (s *memStore) Context(_ context.Context, _ string) ([]agent.Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]agent.Entry, len(s.entries))
	copy(out, s.entries)
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out, nil
}

func (s *memStore) Compact(_ context.Context, _ string, c agent.Compaction) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	subsumed := map[string]bool{}
	for _, e := range c.Subsumes {
		subsumed[e.ID] = true
	}
	kept := s.entries[:0:0]
	for _, e := range s.entries {
		if !subsumed[e.ID] {
			kept = append(kept, e)
		}
	}
	s.entries = append(kept, c.Marker)
	return nil
}
