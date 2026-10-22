package configdb

import (
	"context"
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
)

// A key's allocation (P56) survives the database. It first did not: the key
// table had no column for it, and the import's verify refused to start.
func TestKeyLimitsSurviveTheDatabase(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	in := &config.Config{
		PriorityGroups: map[string]config.PriorityGroup{"default": {Weight: 1}},
		Keys: map[string]config.KeyPolicy{
			"capped": {Group: "default", Limits: config.LimitSet{
				{USD: 5, Per: "calendar-day", Hard: true},
				{Req: 100, Per: "hour"},
			}},
			"plain": {Group: "default"},
		},
	}
	if err := Write(ctx, db, in); err != nil {
		t.Fatal(err)
	}
	out, err := Read(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	got := out.Keys["capped"].Limits
	if len(got) != 2 || got[0].USD != 5 || !got[0].Hard || got[0].Per != "calendar-day" || got[1].Req != 100 || got[1].Hard {
		t.Errorf("capped limits = %+v", got)
	}
	if len(out.Keys["plain"].Limits) != 0 {
		t.Errorf("plain gained limits: %+v", out.Keys["plain"].Limits)
	}
}
