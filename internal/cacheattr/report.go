package cacheattr

import (
	"strconv"
	"strings"

	"github.com/iodesystems/corrallm/internal/store"
	"github.com/iodesystems/corrallm/internal/sysmem"
)

// SettingsFromCmd reads the cache flags from a model's command. -1 for each
// flag the command does not set: the report then says "not set" and the
// rules use llama.cpp's default, never a constant standing in for the live
// value.
func SettingsFromCmd(cmd string) Settings {
	return Settings{
		CacheRAMMiB:      flagInt(cmd, "--cache-ram", "-cram"),
		CtxCheckpoints:   flagInt(cmd, "--ctx-checkpoints", "--swa-checkpoints"),
		Parallel:         flagInt(cmd, "--parallel", "-np"),
		SlotSimilarity:   flagFloat(cmd, "--slot-prompt-similarity", "-sps"),
		MemAvailableMiB:  -1,
		CacheEntries:     -1,
		CacheBytes:       -1,
		EvictionsPerHour: -1,
	}
}

// flagValue is the value given to one of names: "--x N" or "--x=N"; the last
// one wins, as it does for llama-server.
func flagValue(cmd string, names ...string) (string, bool) {
	f := strings.Fields(cmd)
	val, ok := "", false
	for i, tok := range f {
		for _, n := range names {
			switch {
			case tok == n && i+1 < len(f):
				val, ok = f[i+1], true
			case strings.HasPrefix(tok, n+"="):
				val, ok = tok[len(n)+1:], true
			}
		}
	}
	return val, ok
}

func flagInt(cmd string, names ...string) int {
	v, ok := flagValue(cmd, names...)
	if !ok {
		return -1
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return -1
	}
	return n
}

func flagFloat(cmd string, names ...string) float64 {
	v, ok := flagValue(cmd, names...)
	if !ok {
		return -1
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return -1
	}
	return f
}

// Latest is what the report reads from a live poller; nil in the CLI.
type Latest interface {
	Latest(served string) (entries, bytes int64, evictionsPerHour float64, ok bool)
}

// Load builds a report from the store: the window's rows and events, the
// model's flags from cmd, MemAvailable from this host (the backend's host
// when the report runs where the model does), and the poller's counters when
// one is given.
func Load(st *store.Store, served, cmd string, fromMS, toMS int64, by string, live Latest) (Report, error) {
	rows, err := st.CacheRows(served, fromMS, toMS)
	if err != nil {
		return Report{}, err
	}
	events, err := st.BackendEvents(served, fromMS, toMS)
	if err != nil {
		return Report{}, err
	}
	s := SettingsFromCmd(cmd)
	if m, err := sysmem.Probe(); err == nil {
		s.MemAvailableMiB = m.AvailableBytes >> 20
	}
	if live != nil {
		if e, b, eph, ok := live.Latest(served); ok {
			s.CacheEntries, s.CacheBytes, s.EvictionsPerHour = e, b, eph
		}
	}
	return BuildReport(served, fromMS, toMS, rows, events, s, by), nil
}
