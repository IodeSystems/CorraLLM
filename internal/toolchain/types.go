// Package toolchain tracks the PROGRAMS that run the models — which tool is
// installed on which host, at what version, and how far behind its pin — and
// runs the recipes that probe and (from P25c) build them.
//
// It is the same split the rest of corrallm uses for anything that spans hosts:
// the recipe is a shell script that runs WHERE the tool lives, and everything
// that decides anything stays here. A host answers questions about itself; it
// does not hold the registry, and the agent is told what to look at on every
// call rather than being configured with a copy of `tools:` that could drift.
package toolchain

// Verb is one thing a recipe can be asked to do.
//
// They are separate verbs rather than one "status" because their costs differ
// by orders of magnitude, and the scheduled path must be able to ask only the
// cheap question. Probe is a fork and an exec; Upstream is one network round
// trip; Preflight is a handful of `command -v`; InstallDeps mutates the system;
// Build is twenty minutes of CPU and GPU.
type Verb string

const (
	// VerbProbe answers "what is installed here right now".
	VerbProbe Verb = "probe"
	// VerbUpstream answers "has the pin moved" in one `git ls-remote`.
	VerbUpstream Verb = "upstream"
	// VerbPreflight answers "could this host build it, and what is missing".
	// Seconds, and never compiles.
	VerbPreflight Verb = "preflight"
	// VerbInstallDeps installs what Preflight said was missing. Mutates the
	// system, is refused unless the agent was started with --allow-install-deps,
	// and is never scheduled.
	VerbInstallDeps Verb = "install-deps"
	// VerbBuild compiles and installs. P25c/P25d; the recipes refuse it today.
	VerbBuild Verb = "build"
)

// Spec is everything a recipe needs to answer about one tool on one host.
//
// It travels to the host on every call. The agent stores none of it, which is
// what keeps there from being a second copy of the registry that can disagree
// with config — the primary is the only place a tool is declared.
type Spec struct {
	// Name is the tool's key in `tools:`, e.g. "llama.cpp".
	Name string `json:"name"`
	// Recipe is the script to run. Already defaulted from Name by the caller.
	Recipe string `json:"recipe"`
	// URL and Ref are the pin, for Upstream.
	URL string `json:"url,omitempty"`
	Ref string `json:"ref,omitempty"`
	// Bin is the executable to ask for a version, relative to the install dir.
	Bin string `json:"bin,omitempty"`
	// Prefix is where a MANAGED install lives (src/ and bin/ beneath it).
	Prefix string `json:"prefix,omitempty"`
	// InstalledAt ADOPTS an install corrallm does not own: probe reads it and
	// nothing writes to it. Mutually exclusive with a meaningful Prefix.
	InstalledAt string `json:"installedAt,omitempty"`
}

// Probe is what a host reports about an installed tool.
type Probe struct {
	Present bool   `json:"present"`
	Path    string `json:"path"`
	// Version is human-facing and may be empty on a tool that cannot report one.
	Version string `json:"version"`
	// Commit is the revision, when it can be established.
	Commit string `json:"commit"`
	// Source says WHERE the version came from, and is the honest half of this
	// struct: "binary" (the tool told us), "stamp" (we remember building it), or
	// empty, which means present-but-unidentifiable.
	//
	// Empty is a real, expected state, not an error. ninfer has no --version at
	// all, so a ninfer built by hand outside corrallm genuinely cannot be
	// identified, and reporting a made-up version for it would make the registry
	// lie about precisely the tool whose version is hardest to establish.
	Source string `json:"source"`
	// Stamp is the raw build stamp, when there is one.
	Stamp string `json:"stamp"`
	Error string `json:"error,omitempty"`
}

// Identified reports whether the version can be trusted as a statement about
// what is installed, rather than an absence.
func (p Probe) Identified() bool { return p.Present && p.Source != "" }

// Upstream is the drift answer.
type Upstream struct {
	Ref        string `json:"ref"`
	RemoteHead string `json:"remoteHead"`
	// Local is the installed revision — from the stamp when corrallm built it,
	// and from the binary's own banner when it did not. That second path is what
	// makes drift visible on an ADOPTED install: llama-server prints its short
	// commit, so a build corrallm never performed can still be told it is behind.
	Local string `json:"local"`
	// Behind is false when either side is unknown. An unknown is not a "no",
	// but reporting drift we cannot demonstrate would put a permanent
	// out-of-date badge on every tool that cannot identify itself.
	Behind bool   `json:"behind"`
	Error  string `json:"error,omitempty"`
}

// Preflight is "could this host build it".
type Preflight struct {
	OK bool `json:"ok"`
	// Runnable is deliberately separate from OK: buildable and runnable are
	// different questions with different answers on the same box. nvcc happily
	// cross-compiles ninfer for sm_120a on a host whose GPUs cannot run it, and
	// box1 — a 5090 beside a 3080 — can build it and can only run it on one card.
	Runnable bool     `json:"runnable"`
	Missing  []string `json:"missing"`
	Packages []string `json:"packages"`
	// Commands is what install-deps would run, verbatim. Reported even when
	// installing is not allowed, because the operator can then run it by hand.
	Commands []string `json:"commands"`
	Notes    []string `json:"notes"`
	Error    string   `json:"error,omitempty"`
}

// InstallDeps is the result of installing what Preflight found missing.
type InstallDeps struct {
	OK  bool     `json:"ok"`
	Ran []string `json:"ran"`
	// Allowed is false when the host refused because it was not started with
	// --allow-install-deps. The commands still come back in Preflight, so the
	// refusal costs the operator a copy-paste rather than an investigation.
	Allowed bool   `json:"allowed"`
	Error   string `json:"error,omitempty"`
}

// State is one tool on one host, as the registry reports it.
type State struct {
	Tool string `json:"tool"`
	Host string `json:"host"`
	// Declared is false for a host that never mentions this tool. Undeclared is
	// NOT unavailable — "ninfer can never run here" and "nobody has said yet"
	// are different facts, and only one of them is a bug.
	Declared bool `json:"declared"`
	// Adopted means this host's entry points at an install corrallm does not
	// own and will never write to.
	Adopted bool      `json:"adopted"`
	Probe   *Probe    `json:"probe,omitempty"`
	Drift   *Upstream `json:"drift,omitempty"`
	// Error is a failure to ASK — the host was unreachable, its agent is too
	// old, the recipe crashed. Distinct from a probe that answered "not
	// present", which is an answer.
	Error string `json:"error,omitempty"`
}
