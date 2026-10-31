package configdb

import (
	"context"
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
)

// A key's guardrails and the named guardrails both survive a save: the key's
// in its own column, the named ones as a top-level scalar.
func TestGuardrailsSurviveTheDatabase(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	if err := Apply(ctx, db); err != nil {
		t.Fatal(err)
	}
	c := policyConfig()
	c.Guardrails = map[string]config.Guardrail{"violence": {Model: "guard", Policy: "p", Complete: 0.7, Status: 451}}
	kp := c.Keys["sk-aw4"]
	kp.Guardrails = map[string]config.GuardMode{"violence": config.GuardHard, "pii": config.GuardOptIn}
	c.Keys["sk-aw4"] = kp
	if err := Write(ctx, db, c); err != nil {
		t.Fatal(err)
	}
	got, err := Read(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if g := got.Keys["sk-aw4"].Guardrails; g["violence"] != config.GuardHard || g["pii"] != config.GuardOptIn {
		t.Errorf("key guardrails = %v", g)
	}
	if g := got.Guardrails["violence"]; g.Model != "guard" || g.Complete != 0.7 || g.Status != 451 {
		t.Errorf("named guardrail = %+v", g)
	}
	if g := got.Keys["yscr"].Guardrails; len(g) != 0 {
		t.Errorf("yscr got guardrails %v", g)
	}
}
