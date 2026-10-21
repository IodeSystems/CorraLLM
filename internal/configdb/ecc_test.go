package configdb

import (
	"context"
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
)

// A server's `ecc:` override and `residency.nonEccMaxAge` have no columns: they
// ride in the server remainder and the scalar table. Losing either on a config
// write would silently change which models get reloaded — an override saying
// "this card is ECC" dropping back to "no ECC", or a disabled refresh turning
// itself back on.
func TestECCSettingsSurviveTheStore(t *testing.T) {
	ctx := context.Background()
	src := &Source{DB: openDB(t)}

	in := &config.Config{
		Servers: map[string]config.Server{"box1": {
			Pools: map[string]string{"gpu0": "31654MiB", "system": "64GB"},
			ECC:   map[string]bool{"gpu0": true, "system": false},
		}},
		Residency: config.ResidencyConfig{NonECCMaxAge: "0"},
	}
	if err := src.Save(ctx, in); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := src.Load(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	ecc := got.Servers["box1"].ECC
	if v, ok := ecc["gpu0"]; !ok || !v {
		t.Errorf("ecc.gpu0 = %v (present %v), want true", v, ok)
	}
	if v, ok := ecc["system"]; !ok || v {
		t.Errorf("ecc.system = %v (present %v), want an explicit false", v, ok)
	}
	if got.Residency.NonECCMaxAge != "0" {
		t.Errorf("residency.nonEccMaxAge = %q, want \"0\"", got.Residency.NonECCMaxAge)
	}
}
