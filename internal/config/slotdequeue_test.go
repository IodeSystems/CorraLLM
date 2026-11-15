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

func TestKVPoolDefaults(t *testing.T) {
	k := KVPool{Tokens: 1000}
	if k.Reserve() != DefaultKVReserveTokens || k.Bypass() != DefaultKVBypassFor {
		t.Errorf("unset: %d %s, want the defaults", k.Reserve(), k.Bypass())
	}
	k = KVPool{Tokens: 1000, ReserveTokens: 100, BypassFor: "5s"}
	if k.Reserve() != 100 || k.Bypass() != 5*time.Second {
		t.Errorf("set: %d %s, want 100 5s", k.Reserve(), k.Bypass())
	}
}

func TestKVPoolValidate(t *testing.T) {
	const base = `
servers:
  box1:
    pools: { gpu0: 24GB }
models:
  m:
    cmd: llama-server --port ${PORT} --parallel 3 --kv-unified -c 188416
    server: box1
    maxConcurrent: 3
`
	for name, tc := range map[string]struct{ extra, err string }{
		"unset":        {"", ""},
		"tokens only":  {"    kvPool: { tokens: 188416 }\n", ""},
		"all set":      {"    kvPool: { tokens: 188416, reserveTokens: 8192, bypassFor: 10s }\n", ""},
		"no tokens":    {"    kvPool: { reserveTokens: 8192 }\n", "kvPool.tokens"},
		"neg reserve":  {"    kvPool: { tokens: 1000, reserveTokens: -1 }\n", "kvPool.reserveTokens"},
		"huge reserve": {"    kvPool: { tokens: 1000, reserveTokens: 1000 }\n", "kvPool.reserveTokens"},
		"bad bypass":   {"    kvPool: { tokens: 1000, bypassFor: soon }\n", "kvPool.bypassFor"},
		"zero bypass":  {"    kvPool: { tokens: 1000, bypassFor: 0s }\n", "kvPool.bypassFor"},
	} {
		_, err := LoadBytesForTest([]byte(base + tc.extra))
		switch {
		case tc.err == "" && err != nil:
			t.Errorf("%s: unexpected error %v", name, err)
		case tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)):
			t.Errorf("%s: got %v, want an error containing %q", name, err, tc.err)
		}
	}
}
