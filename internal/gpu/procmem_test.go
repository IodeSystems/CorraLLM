package gpu

import "testing"

// unifiedNoVendorTool is an Apple-shaped prober: unified memory, and no
// per-process vendor query. Before groupResidentMiB existed, this combination
// meant "unmeasurable" — which was the whole bug.
type unifiedNoVendorTool struct{}

func (unifiedNoVendorTool) Name() string                   { return "test-unified" }
func (unifiedNoVendorTool) Probe() (Stats, error)          { return Stats{TotalMiB: 49152}, nil }
func (unifiedNoVendorTool) ProbeAll() ([]Stats, error)     { return []Stats{{TotalMiB: 49152}}, nil }
func (unifiedNoVendorTool) ProcVRAM() (map[int]int, error) { return nil, errNoVendorTool }
func (unifiedNoVendorTool) Unified() bool                  { return true }

type discreteNoVendorTool struct{ unifiedNoVendorTool }

func (discreteNoVendorTool) Unified() bool { return false }

var errNoVendorTool = errNoTool{}

type errNoTool struct{}

func (errNoTool) Error() string { return "no vendor tool here" }

// A missing vendor tool must not mean "unmeasurable" on unified memory. This is
// the decision that turned a knowable number into a field humans typed wrong.
func TestPerProcessAvailableOnUnifiedMemoryWithoutAVendorTool(t *testing.T) {
	orig := Default
	t.Cleanup(func() { Default = orig })

	Default = unifiedNoVendorTool{}
	if !PerProcessAvailable() {
		t.Error("unified memory with no vendor tool reported unmeasurable — " +
			"the resident set is the footprint and the OS will say what it is")
	}

	// A discrete card with no working vendor tool genuinely cannot attribute:
	// resident memory there is host RAM, and charging it to a VRAM pool would
	// be a confident wrong answer rather than an honest absence.
	Default = discreteNoVendorTool{}
	if PerProcessAvailable() {
		t.Error("discrete GPU with no vendor tool must stay unmeasurable")
	}
}

// GroupVRAM must route to the resident-set reader when the vendor tool is
// absent AND memory is unified — and must not when it is discrete.
func TestGroupVRAMFallsBackOnlyOnUnifiedMemory(t *testing.T) {
	orig := Default
	t.Cleanup(func() { Default = orig })

	Default = discreteNoVendorTool{}
	if _, err := GroupVRAM(1234); err == nil {
		t.Error("discrete GPU with no vendor tool should report an error, not a substituted number")
	}

	// On unified memory it reaches groupResidentMiB. Whether THAT succeeds is
	// platform-specific (it shells out to ps on darwin and refuses elsewhere),
	// so assert routing rather than a value: a non-darwin build must fail with
	// the resident-set reason, not the vendor-tool one.
	Default = unifiedNoVendorTool{}
	_, err := GroupVRAM(1234)
	if err != nil && err.Error() == errNoVendorTool.Error() {
		t.Error("unified path returned the vendor-tool error — it never tried the resident set")
	}
}
