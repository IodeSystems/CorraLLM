package proc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
)

// benchHold is how long each pause a benchmark sets runs before it lifts on its
// own. Re-set before every phase, so a benchmark longer than this keeps its
// hold, and a daemon that dies mid-benchmark frees production within this.
// Callers see it as the Retry-After of their 429 (writePaused).
const benchHold = 4 * time.Minute

// benchDrain bounds the wait for a benchmark model's in-flight requests to
// finish after it is paused. A long generation on box1 has taken 9 minutes.
const benchDrain = 15 * time.Minute

// benchRequestTimeout bounds one measured request. A healthy 16k prefill is
// ~8 s on the 5090; the CUDA 12.4 build took ~60 s.
const benchRequestTimeout = 10 * time.Minute

// benchGenTokens is how many tokens each measured request decodes.
const benchGenTokens = 256

// BenchSpeed is one build's measured speed on one model: the best of the
// measured runs, in tokens/sec.
type BenchSpeed struct {
	Prefill float64
	Decode  float64
}

// BenchTool measures the build in nextDir against the one in prevDir, on each
// model the tool's `benchmark:` lists that is placed on server, and decides
// whether nextDir may stay.
//
// Each model is PAUSED for the whole run — callers get 429 + Retry-After — and
// stays paused until release is called, so the caller can put the right build
// back in bin/ BEFORE anything respawns on it. release is never nil and must be
// called; it lifts the pauses and reloads the models in the background.
//
// ran=false with an error means nothing could be compared (no benchmark, no
// model here, the OLD build would not load): that says nothing about the new
// build. ran=true with an error means the new build is slower than allowed, or
// would not run where the old one did.
func (m *Manager) BenchTool(ctx context.Context, tool, server, prevDir, nextDir string, log io.Writer) (release func(), ran bool, err error) {
	var held []string
	release = func() {
		for _, name := range held {
			if _, err := m.UnpauseModel(context.Background(), name); err != nil {
				slog.Warn("benchmark: unpause failed", "model", name, "err", err)
				continue
			}
			go func(name string) {
				if _, err := m.LoadModel(context.Background(), name); err != nil {
					slog.Warn("benchmark: reload after benchmark failed", "model", name, "err", err)
				}
			}(name)
		}
	}

	cfg := m.config()
	if cfg == nil {
		return release, false, fmt.Errorf("no config loaded")
	}
	t, ok := cfg.Tools[tool]
	if !ok || t.Benchmark == nil {
		return release, false, fmt.Errorf("tools.%s has no benchmark", tool)
	}
	b := *t.Benchmark
	ref := "${tool:" + tool + "}"
	prompt := benchPrompt(b.Prompt())

	measured := 0
	for _, name := range b.Models {
		mdl, ok := cfg.Models[name]
		if !ok {
			return release, measured > 0, fmt.Errorf("benchmark model %q is not a configured model (use the served name)", name)
		}
		var pl *config.Placement
		for _, p := range mdl.PlacementList() {
			if p.Server == server && strings.Contains(p.Cmd, ref) {
				p := p
				pl = &p
				break
			}
		}
		if pl == nil {
			_, _ = fmt.Fprintf(log, "  %s: not placed on %s with %s; skipped\n", name, server, ref)
			continue
		}
		pm := mdl.ForPlacement(*pl)
		key := mdl.PlacementProcKey(name, *pl)

		// Off the card: production and the benchmark use the same port and
		// the same memory.
		reason := fmt.Sprintf("benchmarking a new %s build", tool)
		hold := func() error {
			_, err := m.PauseModel(name, reason, time.Now().Add(benchHold))
			return err
		}
		if err := hold(); err != nil {
			return release, measured > 0, fmt.Errorf("%s: could not pause: %w", name, err)
		}
		held = append(held, name)
		_, _ = fmt.Fprintf(log, "  %s: paused; waiting for in-flight requests to finish\n", name)
		deadline := time.Now().Add(benchDrain)
		for m.StateOf(key) != StateAbsent {
			if time.Now().After(deadline) {
				return release, measured > 0, fmt.Errorf("%s: still running %s after the pause; not benchmarked", name, benchDrain)
			}
			select {
			case <-ctx.Done():
				return release, measured > 0, ctx.Err()
			case <-time.After(5 * time.Second):
			}
			if err := hold(); err != nil {
				return release, measured > 0, fmt.Errorf("%s: could not keep the pause: %w", name, err)
			}
		}

		run := func(label, dir string) (BenchSpeed, error) {
			if err := hold(); err != nil {
				return BenchSpeed{}, err
			}
			cmd := strings.ReplaceAll(pm.Cmd, ref, dir)
			if m.ExpandCmd != nil {
				var err error
				if cmd, err = m.ExpandCmd(ctx, cmd, server); err != nil {
					return BenchSpeed{}, err
				}
			}
			tm := pm
			tm.Cmd = cmd
			var sp BenchSpeed
			_, err := m.TrialWith(ctx, "bench-"+name, tm, func(e TrialEvent) {
				if e.Stage != TrialLog && e.Msg != "" && !e.OK {
					_, _ = fmt.Fprintf(log, "    [%s] %s\n", e.Stage, e.Msg)
				}
			}, func(ctx context.Context, base string) error {
				var err error
				sp, err = benchRuns(ctx, base, prompt)
				return err
			})
			if err == nil {
				_, _ = fmt.Fprintf(log, "  %s on %s build: prefill %.0f tok/s, decode %.1f tok/s\n", name, label, sp.Prefill, sp.Decode)
			}
			return sp, err
		}

		old, err := run("current", prevDir)
		if err != nil {
			return release, measured > 0, fmt.Errorf("%s: the CURRENT build did not benchmark, so there is nothing to compare against: %w", name, err)
		}
		cur, err := run("new", nextDir)
		if err != nil {
			return release, true, fmt.Errorf("%s: the new build failed where the current one ran: %w", name, err)
		}
		measured++

		floor := 1 - b.MaxRegression()
		tp, tg := cur.Prefill/old.Prefill, cur.Decode/old.Decode
		_, _ = fmt.Fprintf(log, "  %s: new/current prefill %.2f, decode %.2f (floor %.2f)\n", name, tp, tg, floor)
		if tp < floor || tg < floor {
			return release, true, fmt.Errorf("%s is slower on the new build: prefill %.0f -> %.0f tok/s (%+.0f%%), decode %.1f -> %.1f tok/s (%+.0f%%); allowed -%.0f%%",
				name, old.Prefill, cur.Prefill, (tp-1)*100, old.Decode, cur.Decode, (tg-1)*100, b.MaxRegression()*100)
		}
	}
	if measured == 0 {
		return release, false, fmt.Errorf("no benchmark model is placed on %s", server)
	}
	return release, true, nil
}

// benchRuns warms the backend with a short request, then measures the prompt
// twice and keeps the best of each rate: the first long request after a load
// pays one-time costs that are not the build's speed.
func benchRuns(ctx context.Context, base, prompt string) (BenchSpeed, error) {
	if _, err := benchOnce(ctx, base, "Say hello.", 16); err != nil {
		return BenchSpeed{}, fmt.Errorf("warm-up: %w", err)
	}
	var best BenchSpeed
	for range 2 {
		s, err := benchOnce(ctx, base, prompt, benchGenTokens)
		if err != nil {
			return BenchSpeed{}, err
		}
		best.Prefill = max(best.Prefill, s.Prefill)
		best.Decode = max(best.Decode, s.Decode)
	}
	if best.Prefill <= 0 || best.Decode <= 0 {
		return BenchSpeed{}, fmt.Errorf("the backend reported no timings (not a llama-server completion endpoint?)")
	}
	return best, nil
}

// benchOnce sends one greedy, uncached completion to llama-server's native
// endpoint and reads its own timings. Greedy and fixed-length so both builds do
// identical work: same tokens, same draft acceptance.
func benchOnce(ctx context.Context, base, prompt string, n int) (BenchSpeed, error) {
	body, _ := json.Marshal(map[string]any{
		"prompt": prompt, "n_predict": n, "temperature": 0,
		"cache_prompt": false, "ignore_eos": true,
	})
	ctx, cancel := context.WithTimeout(ctx, benchRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/completion", bytes.NewReader(body))
	if err != nil {
		return BenchSpeed{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return BenchSpeed{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return BenchSpeed{}, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out struct {
		Timings struct {
			PromptPerSecond    float64 `json:"prompt_per_second"`
			PredictedPerSecond float64 `json:"predicted_per_second"`
		} `json:"timings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return BenchSpeed{}, err
	}
	return BenchSpeed{Prefill: out.Timings.PromptPerSecond, Decode: out.Timings.PredictedPerSecond}, nil
}

// benchPrompt builds a deterministic prompt of roughly n tokens. Each section
// is ~100 tokens of plain English (measured on Qwen3.8's tokenizer: 330
// sections = 32,916 tokens), numbered so no two are identical.
func benchPrompt(n int) string {
	const para = "The scheduler admits requests by priority group and fair share, spilling to the next backend when a " +
		"lane is saturated and queueing with a bounded wait otherwise. "
	var sb strings.Builder
	for i := 0; i < max(1, n/100); i++ {
		fmt.Fprintf(&sb, "Section %d: %s%s%s\n", i, para, para, para)
	}
	sb.WriteString("\nSummarise the scheduling policy above.")
	return sb.String()
}
