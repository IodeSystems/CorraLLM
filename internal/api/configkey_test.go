package api

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
)

// managedConfig writes a config the editor is willing to rewrite. corrallm
// refuses to rewrite a hand-written file so it cannot eat an operator's
// comments; tests have to opt in the same way a real box does.
func managedConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(p, []byte("# MANAGED CONFIG\n"+body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func putYAML(t *testing.T, h *Handlers, kind, name, yaml string) error {
	t.Helper()
	in := &PutEntryYAMLInput{Kind: kind, Name: name}
	in.Body.YAML = yaml
	_, err := h.PutEntryYAML(context.Background(), in)
	return err
}

// Enrollment: a key seen in traffic becomes a managed key by being assigned a
// group. This is the write half of the roster — without it, key→group was
// hand-edited YAML and a restart, the only part of the scheduling model with no
// management surface.
func TestEnrolAKeyByAssigningItAGroup(t *testing.T) {
	path := managedConfig(t, "priorityGroups:\n  batch:\n    weight: 1\n")
	h := &Handlers{ConfigPath: path}
	h.SetConfig(&config.Config{
		PriorityGroups: map[string]config.PriorityGroup{"batch": {Weight: 1}},
	})

	if err := putYAML(t, h, "key", "newcomer", "batch\n"); err != nil {
		t.Fatalf("enrolling a key should succeed: %v", err)
	}
	saved, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Keys["newcomer"] != "batch" {
		t.Fatalf("keys = %v, want newcomer→batch persisted", saved.Keys)
	}
	name, g, recognized := saved.ResolveGroupRecognized("newcomer")
	if !recognized || name != "batch" || g.EffectiveWeight() != 1 {
		t.Errorf("resolved %s/%d recognized=%v, want batch/1/true", name, g.EffectiveWeight(), recognized)
	}
}

// A typo'd group must FAIL rather than succeed quietly. ResolveGroup falls back
// to the fallback lane, so an accepted typo looks like a successful assignment
// and silently leaves the caller at weight 1 — the exact silent-default failure
// the roster exists to end.
func TestAssigningAnUnknownGroupIsRejected(t *testing.T) {
	path := managedConfig(t, "priorityGroups:\n  batch:\n    weight: 1\n")
	h := &Handlers{ConfigPath: path}
	h.SetConfig(&config.Config{
		PriorityGroups: map[string]config.PriorityGroup{"batch": {Weight: 1}},
	})

	err := putYAML(t, h, "key", "newcomer", "bathc\n")
	if err == nil {
		t.Fatal("a key assigned to a nonexistent group must be rejected")
	}
	if !strings.Contains(err.Error(), "no priority group") {
		t.Errorf("the error must name the problem, got: %v", err)
	}
	saved, _ := config.Load(path)
	if _, ok := saved.Keys["newcomer"]; ok {
		t.Error("a rejected assignment must not persist")
	}
}
