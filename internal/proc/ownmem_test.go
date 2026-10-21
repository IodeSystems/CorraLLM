package proc

import (
	"errors"
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/host"
)

// memHandle is a local backend that reports its memory.
type memHandle struct {
	host.Handle
	dev  map[string]int
	anon int64
	err  error
}

func (h memHandle) MemoryByDeviceMiB() (map[string]int, error) { return h.dev, h.err }
func (h memHandle) AnonBytes() (int64, error)                  { return h.anon, h.err }

// agentMemHandle is the same, run by an agent: never counted as ours.
type agentMemHandle struct{ memHandle }

func (agentMemHandle) AgentID() string { return "a" }

func TestLocalMemorySumsOwnLocalBackends(t *testing.T) {
	m := NewManager(&config.Config{})
	m.procs = map[string]*Process{
		"a": {handle: memHandle{dev: map[string]int{"GPU-1": 100, "GPU-2": 5}, anon: 1000}},
		"b": {handle: memHandle{dev: map[string]int{"GPU-1": 20}, anon: 200}},
		"c": {handle: memHandle{err: errors.New("cannot attribute")}},
		"d": {handle: agentMemHandle{memHandle{dev: map[string]int{"GPU-1": 7000}, anon: 7000}}},
		"e": {}, // not started
	}
	got := m.LocalMemory()
	if got.DeviceMiB["GPU-1"] != 120 || got.DeviceMiB["GPU-2"] != 5 || got.AnonBytes != 1200 {
		t.Fatalf("LocalMemory = %+v", got)
	}
}
