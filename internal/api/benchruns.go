package api

import (
	"context"
	"os"
	"sort"

	"github.com/iodesystems/corrallm/internal/bench/task"
	"github.com/iodesystems/corrallm/internal/store"
)

// Three ways into the same rows.
//
// bench_probe_results is keyed (run, model, probe, arm), and which of those you
// hold fixed is the whole question being asked:
//
//   - fix the RUN     -> "what did this bench actually do?"
//   - fix the MODEL   -> "how has this model moved between runs?"
//   - fix the PROBE   -> "is this test measuring anything?"
//
// Only the model-fixed slice existed. The other two are not reachable from it:
// every prior query takes a model as a required argument, so a caller had to
// already know the participants before it could ask who they were. A probe that
// every model fails is evidence about the PROBE, and no amount of per-model
// views will show it, because each one sees a single row of that column.

// --- run index ---------------------------------------------------------------

// BenchRunsInput bounds the run index.
type BenchRunsInput struct {
	Limit int `query:"limit" doc:"How many runs, newest first (default 50)."`
}

// BenchRunSummaryView is one run as a whole.
type BenchRunSummaryView struct {
	RunID string `json:"runId"`
	At    int64  `json:"at"`
	Host  string `json:"host,omitempty"`
	// HasArtifacts reports whether transcripts and journals are still on disk
	// for this run. A run keeps its scores in SQLite forever but its replay only
	// as long as out/<ts>/ survives, and a UI offering a drill-in that cannot
	// resolve is worse than one that greys it out.
	HasArtifacts bool `json:"hasArtifacts"`

	Models   int     `json:"models"`
	Probes   int     `json:"probes"`
	Rows     int     `json:"rows" doc:"Probe×arm rows recorded; exceeds probes when the run ran A/B arms."`
	Passed   int     `json:"passed"`
	Skipped  int     `json:"skipped"`
	Score    float64 `json:"score" doc:"Passed / (rows - skipped). Skipped rows are excluded, not counted as failures."`
	WallMSum int64   `json:"wallMsSum"`
}

// BenchRunsOutput is the run index.
type BenchRunsOutput struct {
	Body struct {
		Runs []BenchRunSummaryView `json:"runs"`
	}
}

// BenchRuns lists benchmark runs, newest first.
func (h *Handlers) BenchRuns(ctx context.Context, in *BenchRunsInput) (*BenchRunsOutput, error) {
	out := &BenchRunsOutput{}
	out.Body.Runs = []BenchRunSummaryView{}
	if h.Store == nil {
		return out, nil
	}
	rows, err := h.Store.BenchRuns(ctx, in.Limit)
	if err != nil {
		return out, err
	}
	for _, r := range rows {
		v := BenchRunSummaryView{
			RunID: r.RunID, At: r.At, Host: r.Host,
			HasArtifacts: r.OutDir != "",
			Models:       r.Models, Probes: r.Probes, Rows: r.Rows,
			Passed: r.Passed, Skipped: r.Skipped, WallMSum: r.WallMSum,
		}
		// Skipped rows are excluded from the denominator rather than counted as
		// failures: a model that cannot serve a probe has not failed it, and
		// scoring it as such is what made the old aggregate rank an STT model
		// above a chat model.
		if scored := r.Rows - r.Skipped; scored > 0 {
			v.Score = float64(r.Passed) / float64(scored)
		}
		out.Body.Runs = append(out.Body.Runs, v)
	}
	return out, nil
}

// --- one run, every model ----------------------------------------------------

// BenchRunDetailInput selects a run, optionally narrowed to one model.
type BenchRunDetailInput struct {
	RunID string `query:"runId" required:"true"`
	Model string `query:"model" doc:"Narrow to one model; omit for every model in the run."`
}

// BenchRunModelView is one model's showing in one run.
type BenchRunModelView struct {
	Model        string           `json:"model"`
	Score        float64          `json:"score" doc:"Stage pass rate over the probes this model was a candidate for."`
	Stages       int              `json:"stages"`
	StagesPassed int              `json:"stagesPassed"`
	Probes       int              `json:"probes"`
	Skipped      int              `json:"skipped"`
	WallMS       int64            `json:"wallMs"`
	Detail       []BenchProbeView `json:"detail"`
}

// BenchRunDetailOutput is a run broken out by model.
type BenchRunDetailOutput struct {
	Body struct {
		RunID        string              `json:"runId"`
		At           int64               `json:"at"`
		Host         string              `json:"host,omitempty"`
		HasArtifacts bool                `json:"hasArtifacts"`
		Models       []BenchRunModelView `json:"models"`
	}
}

// BenchRunDetail returns what one run did, model by model and probe by probe.
func (h *Handlers) BenchRunDetail(ctx context.Context, in *BenchRunDetailInput) (*BenchRunDetailOutput, error) {
	out := &BenchRunDetailOutput{}
	out.Body.RunID = in.RunID
	out.Body.Models = []BenchRunModelView{}
	if h.Store == nil || in.RunID == "" {
		return out, nil
	}
	if run, ok, err := h.Store.BenchRunFor(ctx, in.RunID); err == nil && ok {
		out.Body.Host, out.Body.HasArtifacts = run.Host, run.OutDir != ""
	}
	rows, err := h.Store.BenchProbeResultsForRun(ctx, in.RunID)
	if err != nil {
		return out, err
	}

	byModel := map[string][]store.BenchProbeResult{}
	var modelOrder []string
	for _, r := range rows {
		if in.Model != "" && r.Model != in.Model {
			continue
		}
		if r.At > out.Body.At {
			out.Body.At = r.At
		}
		if _, seen := byModel[r.Model]; !seen {
			modelOrder = append(modelOrder, r.Model)
		}
		byModel[r.Model] = append(byModel[r.Model], r)
	}
	for _, m := range modelOrder {
		out.Body.Models = append(out.Body.Models, buildRunModelView(m, byModel[m]))
	}
	// Best first: the run index is read to find the outlier, and an alphabetical
	// list buries it.
	sort.SliceStable(out.Body.Models, func(i, j int) bool {
		return out.Body.Models[i].Score > out.Body.Models[j].Score
	})
	return out, nil
}

// buildRunModelView folds one model's rows into probes-with-arms plus a rollup.
func buildRunModelView(model string, rows []store.BenchProbeResult) BenchRunModelView {
	v := BenchRunModelView{Model: model, Detail: []BenchProbeView{}}
	byProbe := map[string][]store.BenchProbeResult{}
	var probeOrder []string
	for _, r := range rows {
		if _, seen := byProbe[r.Probe]; !seen {
			probeOrder = append(probeOrder, r.Probe)
		}
		byProbe[r.Probe] = append(byProbe[r.Probe], r)
	}
	for _, p := range probeOrder {
		pv := buildProbeView(p, byProbe[p])
		v.Detail = append(v.Detail, pv)
		v.Probes++
		if pv.Skipped {
			v.Skipped++
			continue
		}
		v.Stages += pv.Stages
		v.StagesPassed += pv.StagesPassed
		for _, a := range pv.Arms {
			v.WallMS += a.WallMS
		}
	}
	if v.Stages > 0 {
		v.Score = float64(v.StagesPassed) / float64(v.Stages)
	}
	return v
}

// buildProbeView folds one probe's arm rows into a BenchProbeView, taking the
// headline numbers from the baseline arm so an added arm cannot move the score.
func buildProbeView(probe string, rows []store.BenchProbeResult) BenchProbeView {
	pv := BenchProbeView{Probe: probe, Arms: armsFor(rows)}
	if len(rows) > 0 {
		pv.Class = rows[0].Class
	}
	passSeen, failSeen := false, false
	for _, a := range pv.Arms {
		if a.Skipped {
			continue
		}
		if a.Pass {
			passSeen = true
		} else {
			failSeen = true
		}
		if a.IsBaseline {
			pv.Score, pv.Stages, pv.StagesPassed = a.Score, a.Stages, a.StagesPassed
			pv.Pass, pv.Note = a.Pass, a.Note
		}
	}
	// A probe every arm skipped is skipped, not failed.
	pv.Skipped = !passSeen && !failSeen
	if pv.Skipped {
		for _, a := range pv.Arms {
			if a.SkipReason != "" {
				pv.SkipReason = a.SkipReason
				break
			}
		}
	}
	pv.Disagreement = passSeen && failSeen
	return pv
}

// --- one probe, every model --------------------------------------------------

// BenchProbeHistoryInput selects a probe.
type BenchProbeHistoryInput struct {
	Probe string `query:"probe" required:"true"`
	Limit int    `query:"limit" doc:"Row cap across all runs (default 500)."`
}

// BenchProbeRunView is one (run, model) observation of a probe.
type BenchProbeRunView struct {
	RunID string `json:"runId"`
	Model string `json:"model"`
	At    int64  `json:"at"`
	BenchProbeView
}

// BenchProbeHistoryOutput is a probe's description plus every result for it.
type BenchProbeHistoryOutput struct {
	Body struct {
		Probe string `json:"probe"`
		// Catalog is the probe's own description — what it seeds, asks and
		// asserts. Nil when the probe is not in the current library, which is
		// itself the answer to "why does this old result have no description":
		// the probe was renamed or removed, and its results outlived it.
		Catalog *task.CatalogEntry `json:"catalog,omitempty"`
		// Models counts distinct models that produced a non-skipped result.
		Models int `json:"models"`
		// PassRate is over non-skipped observations. A probe no model has ever
		// passed is a probe to go and read, not a hard one.
		PassRate float64             `json:"passRate"`
		Runs     []BenchProbeRunView `json:"runs"`
	}
}

// BenchProbeHistory returns one probe across every model and run that ran it.
func (h *Handlers) BenchProbeHistory(ctx context.Context, in *BenchProbeHistoryInput) (*BenchProbeHistoryOutput, error) {
	out := &BenchProbeHistoryOutput{}
	out.Body.Probe = in.Probe
	out.Body.Runs = []BenchProbeRunView{}
	if in.Probe == "" {
		return out, nil
	}
	if e, ok := h.catalogEntry(in.Probe); ok {
		out.Body.Catalog = &e
	}
	if h.Store == nil {
		return out, nil
	}
	rows, err := h.Store.BenchProbeHistory(ctx, in.Probe, in.Limit)
	if err != nil {
		return out, err
	}

	type rmKey struct{ runID, model string }
	byRM := map[rmKey][]store.BenchProbeResult{}
	var order []rmKey
	at := map[rmKey]int64{}
	for _, r := range rows {
		k := rmKey{r.RunID, r.Model}
		if _, seen := byRM[k]; !seen {
			order = append(order, k)
		}
		byRM[k] = append(byRM[k], r)
		if r.At > at[k] {
			at[k] = r.At
		}
	}
	models := map[string]bool{}
	scored, passed := 0, 0
	for _, k := range order {
		pv := buildProbeView(in.Probe, byRM[k])
		out.Body.Runs = append(out.Body.Runs, BenchProbeRunView{
			RunID: k.runID, Model: k.model, At: at[k], BenchProbeView: pv,
		})
		if pv.Skipped {
			continue
		}
		models[k.model] = true
		scored++
		if pv.Pass {
			passed++
		}
	}
	out.Body.Models = len(models)
	if scored > 0 {
		out.Body.PassRate = float64(passed) / float64(scored)
	}
	return out, nil
}

// catalogEntry looks one probe up in the resolved library.
//
// Best-effort by design: the catalog describes what WOULD run now, and a result
// may predate the current library. A missing entry means the description is
// unavailable, never that the results are invalid.
func (h *Handlers) catalogEntry(probe string) (task.CatalogEntry, bool) {
	entries, err := task.Catalog(h.BenchProbes, os.TempDir())
	if err != nil {
		return task.CatalogEntry{}, false
	}
	for _, e := range entries {
		if e.Name == probe || e.Dir == probe {
			return e, true
		}
	}
	return task.CatalogEntry{}, false
}
