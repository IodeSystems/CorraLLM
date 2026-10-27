package configdb

import (
	"context"
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
)

// A key's allow patterns and its on/off switch (P63) survive the database.
// Without a column they exist only until the daemon restarts, which is exactly
// how the allocation bug showed up (keylimits_test.go).
func TestKeyCredentialsAndDisabledSurviveTheDatabase(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	in := &config.Config{
		PriorityGroups: map[string]config.PriorityGroup{"default": {Weight: 1}},
		Keys: map[string]config.KeyPolicy{
			"gated": {Group: "default", Disabled: true, Credentials: []config.KeyCredential{
				{Credential: "cred:fireworks/*", Limits: config.LimitSet{{USD: 2, Per: "calendar-day", Hard: true}}},
				{Credential: "cred:fireworks/paid", Limits: config.LimitSet{{USD: 20, Per: "calendar-day", Hard: true}}},
				{Credential: "cred:groq/main"},
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
	got := out.Keys["gated"]
	if !got.Disabled {
		t.Error("the off switch was lost")
	}
	if len(got.Credentials) != 3 {
		t.Fatalf("patterns = %+v", got.Credentials)
	}
	// Sorted by pattern, so two saves of the same config produce the same rows
	// and the revision history does not show a change that did not happen.
	if got.Credentials[0].Credential != "cred:fireworks/*" || got.Credentials[1].Credential != "cred:fireworks/paid" {
		t.Errorf("pattern order = %v", []string{got.Credentials[0].Credential, got.Credentials[1].Credential})
	}
	if ls := got.Credentials[1].Limits; len(ls) != 1 || ls[0].USD != 20 || !ls[0].Hard {
		t.Errorf("the narrow pattern's budget = %+v", ls)
	}
	if ls := got.Credentials[2].Limits; len(ls) != 0 {
		t.Errorf("an unbudgeted pattern gained limits: %+v", ls)
	}
	plain := out.Keys["plain"]
	if plain.Disabled || len(plain.Credentials) != 0 {
		t.Errorf("plain gained state: %+v", plain)
	}
	// And the round trip is stable: a second write of what was read matches.
	if err := Write(ctx, db, out); err != nil {
		t.Fatal(err)
	}
	again, err := Read(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Keys["gated"].Credentials) != 3 || !again.Keys["gated"].Disabled {
		t.Errorf("second round trip = %+v", again.Keys["gated"])
	}
}
