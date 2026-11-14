package config

import (
	"strings"
	"testing"
	"time"
)

func TestSlotSkipBound(t *testing.T) {
	if k, w := (Model{}).SlotSkipBound(); k != DefaultSlotSkipMax || w != DefaultSlotSkipWait {
		t.Errorf("unset: %d %s, want the defaults", k, w)
	}
	m := Model{SlotAwareDequeueMaxSkips: 5, SlotAwareDequeueMaxWait: "750ms"}
	if k, w := m.SlotSkipBound(); k != 5 || w != 750*time.Millisecond {
		t.Errorf("set: %d %s, want 5 750ms", k, w)
	}
}

func TestSlotAwareDequeueValidate(t *testing.T) {
	const base = `
servers:
  box1:
    pools: { gpu0: 24GB }
models:
  m:
    cmd: llama-server --port ${PORT} --parallel 2
    server: box1
    maxConcurrent: 2
    slotAwareDequeue: true
`
	for name, tc := range map[string]struct{ extra, err string }{
		"defaults":   {"", ""},
		"bounds set": {"    slotAwareDequeueMaxSkips: 3\n    slotAwareDequeueMaxWait: 5s\n", ""},
		"bad wait":   {"    slotAwareDequeueMaxWait: soon\n", "slotAwareDequeueMaxWait"},
		"zero wait":  {"    slotAwareDequeueMaxWait: 0s\n", "slotAwareDequeueMaxWait"},
		"neg skips":  {"    slotAwareDequeueMaxSkips: -1\n", "slotAwareDequeueMaxSkips"},
	} {
		c, err := LoadBytesForTest([]byte(base + tc.extra))
		switch {
		case tc.err == "" && err != nil:
			t.Errorf("%s: unexpected error %v", name, err)
		case tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)):
			t.Errorf("%s: got %v, want an error containing %q", name, err, tc.err)
		case tc.err == "" && !c.Models["m"].SlotAwareDequeue:
			t.Errorf("%s: slotAwareDequeue not parsed", name)
		}
	}
}
