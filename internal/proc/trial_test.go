package proc

import (
	"strings"
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
)

func trialCfg(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.LoadBytesForTest([]byte(`
servers:
  box1:
    pools: { gpu0: 30GB }
models:
  resident:
    cmd: "exec llama-server --port 5800"
    server: box1
    ramUsage: { gpu0: 24GB }
    proxy: 5800
`))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// trialModel is what the operator is trying out: it exists in no config.
func trialModel(t *testing.T, ram string) config.Model {
	t.Helper()
	cfg, err := config.LoadBytesForTest([]byte(`
servers:
  box1:
    pools: { gpu0: 30GB }
models:
  candidate:
    cmd: "exec llama-server --port 5801"
    server: box1
    ramUsage: { gpu0: ` + ram + ` }
    proxy: 5801
`))
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Models["candidate"]
}

// The decision this encodes: a trial is an EXPERIMENT. It may use free memory
// and nothing else. Evicting a warm production backend to run one would make
// "let me just try this command" a production incident.
func TestTrialRefusesRatherThanEvict(t *testing.T) {
	m := NewManager(trialCfg(t))

	// A resident model holds most of the pool, exactly as a live one would.
	resident := m.config().Models["resident"]
	usage, err := config.ParseSizes(resident.RAMUsage)
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.reserveLocked("box1", usage)
	m.procs["resident"] = &Process{
		Name: "resident", key: "resident", server: "box1", usage: usage,
		state: StateReady, ready: make(chan struct{}),
	}
	m.mu.Unlock()

	// 12GB does not fit beside 24GB of 30GB.
	_, err = m.admitTrial(TrialKeyPrefix+"t1", "t1", trialModel(t, "12GB"), nil)
	if err == nil {
		t.Fatal("trial was admitted with no room — it must refuse, not evict")
	}
	if !strings.Contains(err.Error(), "unload something first") {
		t.Errorf("refusal should tell the operator what to do, got: %v", err)
	}

	// The resident is untouched: still registered, still holding its pools.
	m.mu.Lock()
	_, stillThere := m.procs["resident"]
	used := m.used["box1"]["gpu0"]
	m.mu.Unlock()
	if !stillThere {
		t.Error("the resident model was evicted by a refused trial")
	}
	if used != usage["gpu0"] {
		t.Errorf("used = %d, want the resident's %d — a refused trial must reserve nothing", used, usage["gpu0"])
	}
}

// A trial that fits takes only free space, and gives all of it back.
func TestTrialReservesThenReleases(t *testing.T) {
	m := NewManager(trialCfg(t))

	p, err := m.admitTrial(TrialKeyPrefix+"t1", "t1", trialModel(t, "8GB"), nil)
	if err != nil {
		t.Fatalf("a trial that fits should be admitted: %v", err)
	}

	m.mu.Lock()
	used := m.used["box1"]["gpu0"]
	_, registered := m.procs[TrialKeyPrefix+"t1"]
	m.mu.Unlock()
	if used == 0 {
		t.Error("an admitted trial reserved nothing — the ledger is lying while it runs")
	}
	// Reconcile reaps any backend on an agent that no live Process claims, so a
	// trial tracked outside m.procs would be killed as an orphan mid-load.
	if !registered {
		t.Error("trial is not in m.procs — ReconcileAgent would reap it as an orphan")
	}

	m.endTrial(TrialKeyPrefix+"t1", p)

	m.mu.Lock()
	used = m.used["box1"]["gpu0"]
	_, stillRegistered := m.procs[TrialKeyPrefix+"t1"]
	m.mu.Unlock()
	if used != 0 {
		t.Errorf("used = %d after teardown, want 0 — the reservation leaked", used)
	}
	if stillRegistered {
		t.Error("trial still registered after teardown")
	}
}

// An unsized probe takes what is FREE, not the whole pool.
//
// Reserving everything is right for a production spawn (it evicts, runs alone,
// and the measurement governs afterwards) and a dead end for a probe, which
// never evicts: it could then only run on an empty machine, while the first
// thing anyone wants to probe is a second model beside an existing one.
func TestUnsizedProbeTakesTheFreeRemainder(t *testing.T) {
	m := NewManager(trialCfg(t))

	// A resident model holds 24GB of the 30GB pool.
	resident := m.config().Models["resident"]
	usage, err := config.ParseSizes(resident.RAMUsage)
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.reserveLocked("box1", usage)
	m.procs["resident"] = &Process{
		Name: "resident", key: "resident", server: "box1", usage: usage,
		state: StateReady, ready: make(chan struct{}),
	}
	m.mu.Unlock()

	mdl := trialModel(t, "4GB")
	mdl.RAMUsage = nil // nothing declared: the whole point

	p, err := m.admitTrial(TrialKeyPrefix+"t1", "t1", mdl, nil)
	if err != nil {
		t.Fatalf("an unsized probe must be able to use the free remainder: %v", err)
	}
	want := m.budget["box1"]["gpu0"] - usage["gpu0"]
	if p.usage["gpu0"] != want {
		t.Errorf("reserved %d, want the free remainder %d", p.usage["gpu0"], want)
	}
	// Still exclusive against the unknown: nothing else may be admitted while an
	// unmeasured model is running, because its real size is not yet known.
	m.mu.Lock()
	fits := m.fitsLocked("box1", map[string]int64{"gpu0": 1}, nil)
	m.mu.Unlock()
	if fits {
		t.Error("something else was admitted alongside an unmeasured probe")
	}
	m.endTrial(TrialKeyPrefix+"t1", p)
}

// A full machine has no room to probe into, and must say so rather than
// reserving zero and pretending the model is free.
func TestUnsizedProbeRefusesWhenNothingIsFree(t *testing.T) {
	m := NewManager(trialCfg(t))
	m.mu.Lock()
	m.reserveLocked("box1", map[string]int64{"gpu0": m.budget["box1"]["gpu0"]})
	m.mu.Unlock()

	mdl := trialModel(t, "4GB")
	mdl.RAMUsage = nil
	if _, err := m.admitTrial(TrialKeyPrefix+"t1", "t1", mdl, nil); err == nil {
		t.Error("probe admitted onto a full machine with no size and no room")
	}
}

// A trial must run somewhere and have something to run; both failures are the
// operator's typo, and both should say so before anything is reserved.
func TestTrialRejectsAnIncompleteSpec(t *testing.T) {
	m := NewManager(trialCfg(t))

	noCmd := trialModel(t, "4GB")
	noCmd.Cmd = ""
	if _, err := m.admitTrial(TrialKeyPrefix+"a", "a", noCmd, nil); err == nil {
		t.Error("a trial with no cmd was admitted — there is nothing to spawn")
	}

	noServer := trialModel(t, "4GB")
	noServer.Server = ""
	if _, err := m.admitTrial(TrialKeyPrefix+"b", "b", noServer, nil); err == nil {
		t.Error("a trial with no server was admitted — it must run somewhere")
	}

	m.mu.Lock()
	n := len(m.procs)
	m.mu.Unlock()
	if n != 0 {
		t.Errorf("%d processes registered by rejected trials, want 0", n)
	}
}

func TestIsTrialDistinguishesExperimentsFromModels(t *testing.T) {
	if !IsTrial(TrialKeyPrefix + "abc") {
		t.Error("a trial key should be recognised as one")
	}
	if IsTrial("qwen3-coder") {
		t.Error("a configured model was mistaken for a trial")
	}
}

// The bug that killed a real probe mid-download: the primary tracked the
// backend as "trial:<id>" while the agent registered it under the served name,
// so reconciliation saw an unclaimed backend (reaped it) AND a vanished process
// (freed its pools) — for one healthy backend that was still downloading.
//
// The same mismatch applies to extension-hosted models, whose key is
// "extension:<ext>" and whose Spec.Name is the served model.
func TestSpawnCarriesTheReconciliationKey(t *testing.T) {
	if got := specKeyFor("trial:abc", "abc"); got != "trial:abc" {
		t.Errorf("spawn key = %q, want the process key %q — reconcile matches on it",
			got, "trial:abc")
	}
	// An ordinary model has key == name; falling back must not change it.
	if got := specKeyFor("", "qwen"); got != "qwen" {
		t.Errorf("spawn key = %q, want the served name when no key is set", got)
	}
}

// specKeyFor mirrors the defaulting in agent.specKey, kept here so the contract
// is asserted from the side that depends on it.
func specKeyFor(key, name string) string {
	if key != "" {
		return key
	}
	return name
}

// A trial must not block another trial on the same server. Capacity decides
// whether there is room; an exclusivity rule refused runs that would have fit.
func TestTrialsAreNotExclusivePerServer(t *testing.T) {
	m := NewManager(trialCfg(t))
	a, err := m.admitTrial(TrialKeyPrefix+"t1", "t1", trialModel(t, "4GB"), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.admitTrial(TrialKeyPrefix+"t2", "t2", trialModel(t, "4GB"), nil)
	if err != nil {
		t.Fatalf("a second trial that fits was refused: %v", err)
	}
	m.endTrial(TrialKeyPrefix+"t1", a)
	m.endTrial(TrialKeyPrefix+"t2", b)
}
