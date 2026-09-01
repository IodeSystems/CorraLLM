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
	h := storeBackedHandlers(t, &config.Config{
		PriorityGroups: map[string]config.PriorityGroup{"batch": {Weight: 1}},
	})

	if err := putYAML(t, h, "key", "newcomer", "batch\n"); err != nil {
		t.Fatalf("enrolling a key should succeed: %v", err)
	}
	saved := reloadStored(t, h, "after enrolling a key")
	if saved.Keys["newcomer"].Group != "batch" {
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
	h := storeBackedHandlers(t, &config.Config{
		PriorityGroups: map[string]config.PriorityGroup{"batch": {Weight: 1}},
	})

	err := putYAML(t, h, "key", "newcomer", "bathc\n")
	if err == nil {
		t.Fatal("a key assigned to a nonexistent group must be rejected")
	}
	if !strings.Contains(err.Error(), "no priority group") {
		t.Errorf("the error must name the problem, got: %v", err)
	}
	saved := reloadStored(t, h, "after a rejected assignment")
	if _, ok := saved.Keys["newcomer"]; ok {
		t.Error("a rejected assignment must not persist")
	}
}

// A key may be written as a policy, not just a group name — otherwise the
// escalation the config and the database both support has no way in.
func TestEnrolAKeyWithEscalations(t *testing.T) {
	h := storeBackedHandlers(t, &config.Config{
		PriorityGroups: map[string]config.PriorityGroup{
			"batch": {Weight: 1}, "interactive": {Weight: 10},
		},
	})
	if err := putYAML(t, h, "key", "sk-aw4", "default: batch\ninteractive: true\n"); err != nil {
		t.Fatalf("enrolling a key with escalations should succeed: %v", err)
	}
	saved := reloadStored(t, h, "after enrolling a policy key")
	pol := saved.Keys["sk-aw4"]
	if pol.Group != "batch" {
		t.Errorf("Group = %q, want batch", pol.Group)
	}
	if !pol.Allow["interactive"] {
		t.Errorf("Allow = %+v, want interactive", pol.Allow)
	}
	// And it resolves: the whole point is that sk-aw4:interactive is honoured.
	if cr := saved.ResolveCaller("sk-aw4:interactive"); cr.GroupName != "interactive" || cr.Denied {
		t.Errorf("resolved %+v, want interactive granted", cr)
	}
}

// An escalation to a group that does not exist is refused HERE, where somebody
// is reading the error, rather than at request time where nothing says why.
func TestEnrolRejectsUnknownEscalation(t *testing.T) {
	h := storeBackedHandlers(t, &config.Config{
		PriorityGroups: map[string]config.PriorityGroup{"batch": {Weight: 1}},
	})
	if err := putYAML(t, h, "key", "sk-aw4", "default: batch\nghost: true\n"); err == nil {
		t.Error("want an error for an escalation to an unknown group")
	}
}

// The bare-string form sets where a key LANDS. It must not silently revoke
// escalations nobody touched.
func TestBareGroupFormKeepsExistingEscalations(t *testing.T) {
	h := storeBackedHandlers(t, &config.Config{
		PriorityGroups: map[string]config.PriorityGroup{
			"batch": {Weight: 1}, "interactive": {Weight: 10},
		},
	})
	if err := putYAML(t, h, "key", "sk-aw4", "default: batch\ninteractive: true\n"); err != nil {
		t.Fatal(err)
	}
	if err := putYAML(t, h, "key", "sk-aw4", "interactive\n"); err != nil {
		t.Fatal(err)
	}
	pol := reloadStored(t, h, "after a bare-group rewrite").Keys["sk-aw4"]
	if pol.Group != "interactive" {
		t.Errorf("Group = %q, want the reassignment to interactive", pol.Group)
	}
	if !pol.Allow["interactive"] {
		t.Errorf("Allow = %+v; a bare-group write must not revoke escalations", pol.Allow)
	}
}
