package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/iodesystems/corrallm/internal/cacheattr"
	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/store"
)

// newCacheCmd is `corrallm cache`: the prompt-cache attribution (P75), read
// from the database directly as `spend` does, so it needs no admin token.
func newCacheCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cache",
		Short: "Prompt-cache attribution: why turns missed the cache, and what to change",
		Args:  cobra.NoArgs,
	}
	var (
		model  string
		since  time.Duration
		by     string
		asJSON bool
	)
	report := &cobra.Command{
		Use:   "report",
		Short: "Misses by class, gap and reload reason over a window, with recommendations (text only)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, st, done, err := spendDB(cmd.Context())
			if err != nil {
				return err
			}
			defer done()
			served, mdl, err := resolveLocalModel(cfg, model)
			if err != nil {
				return err
			}
			now := time.Now().UnixMilli()
			rep, err := cacheattr.Load(st, served, mdl.Cmd, now-since.Milliseconds(), now, by, nil)
			if err != nil {
				return err
			}
			if asJSON {
				raw, err := json.MarshalIndent(rep, "", "  ")
				if err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), string(raw))
				return nil
			}
			_, err = fmt.Fprint(cmd.OutOrStdout(), cacheattr.Text(rep))
			return err
		},
	}
	report.Flags().StringVar(&model, "model", "swift", "served model, or a case-insensitive part of exactly one local model's name")
	report.Flags().DurationVar(&since, "since", 24*time.Hour, "window, back from now")
	report.Flags().StringVar(&by, "by", "class", "table to add: class, gap, caller, hour or affinity")
	report.Flags().BoolVar(&asJSON, "json", false, "print the report as JSON")
	cmd.AddCommand(report)
	return cmd
}

// resolveLocalModel finds the model --model names: the served name exactly,
// else the one local model (a cmd) whose served name contains it, ignoring
// case. "swift" alone is not a served name; "local-swift…" is.
func resolveLocalModel(cfg *config.Config, name string) (string, config.Model, error) {
	if m, ok := cfg.Effective(name); ok {
		return name, m, nil
	}
	want := strings.ToLower(name)
	var hits []string
	for served := range cfg.Models {
		if m, ok := cfg.Effective(served); ok && m.Cmd != "" && strings.Contains(strings.ToLower(served), want) {
			hits = append(hits, served)
		}
	}
	sort.Strings(hits)
	switch len(hits) {
	case 1:
		m, _ := cfg.Effective(hits[0])
		return hits[0], m, nil
	case 0:
		return "", config.Model{}, fmt.Errorf("unknown model %q: no local model's name contains it", name)
	}
	return "", config.Model{}, fmt.Errorf("model %q matches %d local models: %s", name, len(hits), strings.Join(hits, ", "))
}

// newCacheTracker makes the daemon's prompt-cache tracker, seeded from the
// last 24 h. A failed seed costs no-history verdicts until conversations
// come back, never the boot.
func newCacheTracker(st *store.Store, ev cacheattr.Evictions) *cacheattr.Tracker {
	t := cacheattr.NewTracker(cacheattr.DefaultCapBytes, ev)
	if st == nil {
		return t
	}
	now := time.Now()
	from := now.Add(-24 * time.Hour).UnixMilli()
	turns, err := st.LastTurns(from)
	if err != nil {
		slog.Warn("prompt-cache tracker: seed conversations", "err", err)
	}
	events, err := st.BackendEvents("", from, now.UnixMilli()+1)
	if err != nil {
		slog.Warn("prompt-cache tracker: seed backend events", "err", err)
	}
	t.Seed(turns, events)
	slog.Info("prompt-cache tracker seeded", "conversations", t.Len(), "events", len(events))
	return t
}
