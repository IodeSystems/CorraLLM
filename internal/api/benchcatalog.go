package api

import (
	"context"
	"os"
	"strings"

	"github.com/iodesystems/corrallm/internal/bench/task"
)

// The probe CATALOG: what could run, as opposed to what did.
//
// Every other bench endpoint answers a question about results — this model's
// last run, that probe's transcript, which models lack data. None of them can
// answer "what probes exist?", and the difference matters in exactly the case
// you care about: a probe that was never picked up (wrong directory, failed to
// parse, shadowed by a user copy) is indistinguishable, in a results view, from
// a probe that ran and produced nothing. `benchPlan` reports which MODELS lack
// data; it assumes the probe set is known.
//
// It also reports probes that FAIL to load, with the error. A catalog that
// silently omitted the broken ones would recreate the silence it exists to end.

// BenchCatalogInput selects which probe set to describe.
type BenchCatalogInput struct {
	// ProbesDir overrides the server's configured directories. Empty uses the
	// server's own — which is itself empty when the built-in library is being
	// used, so the default answer describes what a no-flag run would execute.
	// Comma-separated, matching --tasks-dir.
	ProbesDir string `query:"probesDir" doc:"Comma-separated probe directories to describe (default: the server's configured ones, or the built-in library)"`
}

// BenchCatalogOutput is the probe library as the runner would resolve it.
type BenchCatalogOutput struct {
	Body struct {
		// Source is where this listing came from: "builtin" when the binary's
		// own library was used, otherwise the directory paths.
		Source string              `json:"source"`
		Probes []task.CatalogEntry `json:"probes"`
		// Classes counts probes per class, so a caller can see the shape of the
		// library (capability vs tooluse vs coding vs adversarial) without
		// tallying it themselves.
		Classes map[string]int `json:"classes"`
		// Invalid counts entries carrying a load error.
		Invalid int `json:"invalid"`
	}
}

// BenchProbeCatalog lists every probe the runner would resolve, described
// without running any of them.
func (h *Handlers) BenchProbeCatalog(ctx context.Context, in *BenchCatalogInput) (*BenchCatalogOutput, error) {
	dirs := task.SplitProbeDirs(in.ProbesDir)
	if len(dirs) == 0 {
		dirs = h.BenchProbeDirs
	}
	out := &BenchCatalogOutput{}
	out.Body.Probes = []task.CatalogEntry{}
	out.Body.Classes = map[string]int{}
	out.Body.Source = strings.Join(dirs, ", ")
	if len(dirs) == 0 {
		out.Body.Source = string(task.SourceBuiltin)
	}

	// Materialize into the OS temp dir: the catalog is a read, and it must not
	// depend on a writable working directory or leave state behind that a run
	// would then disagree with.
	entries, err := task.Catalog(dirs, os.TempDir())
	if err != nil {
		return out, err
	}
	for _, e := range entries {
		out.Body.Probes = append(out.Body.Probes, e)
		if e.Error != "" {
			out.Body.Invalid++
			continue
		}
		out.Body.Classes[e.Class]++
	}
	return out, nil
}
