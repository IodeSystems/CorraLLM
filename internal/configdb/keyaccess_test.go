package configdb

import (
	"context"
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
)

// A key's label, models and budgets (P65) survive the database.
func TestKeyLabelModelsBudgetsSurviveTheDatabase(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	in := &config.Config{
		PriorityGroups: map[string]config.PriorityGroup{"default": {Weight: 1}},
		Keys: map[string]config.KeyPolicy{
			"sk-aw4": {Group: "default", Label: "aw4", Models: []string{"local", "provider:fireworks"},
				Budgets: config.KeyBudgets{"model:*deepseek*": {{USD: 3, Per: "calendar-day", Hard: true}}}},
		},
	}
	if err := Write(ctx, db, in); err != nil {
		t.Fatal(err)
	}
	out, err := Read(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	got := out.Keys["sk-aw4"]
	if got.Label != "aw4" || len(got.Models) != 2 || len(got.Budgets["model:*deepseek*"]) != 1 ||
		!got.Budgets["model:*deepseek*"][0].Hard {
		t.Errorf("read back %+v", got)
	}
}
