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
	// VerbBuilds lists the installed builds a host could roll back to. As cheap
	// as Probe — it reads a directory — and deliberately does not mutate the
	// tree it describes, so it is safe to call from a listing.
	VerbBuilds Verb = "builds"
	// VerbActivate points a tool's bin/ at one of those builds. A rename, not a
	// compile: the whole point is that reverting a bad upstream costs seconds
	// rather than the twenty minutes it took to produce it.
	VerbActivate Verb = "activate"
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
	// URL is the upstream remote and Ref is what the tool TRACKS, for Upstream.
	URL string `json:"url,omitempty"`
	Ref string `json:"ref,omitempty"`
	// Pin holds the tool at one commit, overriding Ref for the checkout.
	//
	// Both travel, not just the effective one: the recipe aligns to the pin and
	// still asks the remote where Ref has got to, so an operator holding a tool
	// back can see how far ahead the branch has run without un-pinning to find
	// out.
	Pin string `json:"pin,omitempty"`
	// Bin is the executable to ask for a version, relative to the install dir.
	Bin string `json:"bin,omitempty"`
	// Prefix is where a MANAGED install lives (src/ and bin/ beneath it).
	Prefix string `json:"prefix,omitempty"`
	// InstalledAt ADOPTS an install corrallm does not own: probe reads it and
	// nothing writes to it. Mutually exclusive with a meaningful Prefix.
	InstalledAt string `json:"installedAt,omitempty"`
	// SelectBuild is the build id VerbActivate should make current. It rides
	// along in the spec rather than in a separate argument because the spec is
	// already the one thing that travels to the host on every call — a second
	// channel for one string would be two things to keep in agreement.
	SelectBuild string `json:"selectBuild,omitempty"`
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
	// Pin is the commit the tool is held at, empty when it tracks Ref freely.
	//
	// When it is set it is also the TARGET: Behind then means "the installed
	// build is not the pinned commit", not "upstream has moved". Those are
	// different questions and only the first one is actionable while pinned —
	// a pinned tool is behind upstream on purpose, and reporting that as drift
	// would put a permanent warning on a deliberate decision.
	Pin string `json:"pin,omitempty"`
	// Local is the installed revision — from the stamp when corrallm built it,
	// and from the binary's own banner when it did not. That second path is what
	// makes drift visible on an ADOPTED install: llama-server prints its short
	// commit, so a build corrallm never performed can still be told it is behind.
	Local string `json:"local"`
	// Behind is false when either side is unknown. An unknown is not a "no",
	// but reporting drift we cannot demonstrate would put a permanent
	// out-of-date badge on every tool that cannot identify itself.
	//
	// The other side is the PIN when there is one and RemoteHead otherwise.
	Behind bool `json:"behind"`
	// Ahead counts nothing — it says only that the tracked ref has moved past
	// the pin, which is the fact a pinned operator wants at a glance ("master
	// is at 34af94cd9, you are holding 0b1bad14f"). Always false when not
	// pinned, where Behind already says it.
	Ahead bool `json:"ahead,omitempty"`
	// PinUnsupported means this host's recipe PREDATES pins, so its answer was
	// computed against the tracked ref and says nothing about the pin.
	//
	// Not a field a recipe sets — it is the primary's inference from what came
	// back. Agents self-update on a build-id mismatch, so between deploying the
	// primary and a host's next heartbeat the two genuinely disagree about what
	// `upstream` means, and the older one answers. Feature-detected rather than
	// version-gated because the protocol deliberately stays at 1: a pinned
	// recipe always reports the pin back, and an old one never can.
	PinUnsupported bool   `json:"pinUnsupported,omitempty"`
	Error          string `json:"error,omitempty"`
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

// Build is the result of compiling and installing a tool.
type Build struct {
	OK bool `json:"ok"`
	// ID is the build directory this landed in, and what Activate takes to put
	// it back later. Reported on a skip too, where it names what is already
	// current.
	ID string `json:"id"`
	// Skipped means the stamp already matched — same HEAD, same patch set, same
	// arch list — so there was nothing to do. Reported rather than hidden,
	// because "it finished in two seconds" should be explicable.
	Skipped bool   `json:"skipped"`
	Head    string `json:"head"`
	Version string `json:"version"`
	Stamp   string `json:"stamp"`
	Seconds int    `json:"seconds"`
	Error   string `json:"error,omitempty"`
}

// BuildEntry is one installed build of a tool on a host.
type BuildEntry struct {
	// ID is the build directory's name: a UTC timestamp and the short commit,
	// so a listing sorts and reads without opening anything.
	ID string `json:"id"`
	// Stamp is the full build stamp (head, patch hash, arch list), which is the
	// only version source for a tool that cannot report one itself.
	Stamp string `json:"stamp"`
	Head  string `json:"head"`
	// At is when the build was installed, unix seconds.
	At int64 `json:"at"`
	// Active means bin/ currently points here.
	Active bool `json:"active"`
}

// BuildList is what a host can roll back to.
type BuildList struct {
	Builds []BuildEntry `json:"builds"`
	Active string       `json:"active"`
	// Versioned is false for a prefix that predates versioned installs — bin/
	// is still a real directory there, and it becomes a build of its own the
	// next time this tool is built. Reported rather than inferred from an empty
	// list, because "nothing to roll back to yet" and "this layout does not
	// track builds" are different answers.
	Versioned bool `json:"versioned"`
	// Keep is the host's retention count, so a UI can say what will be pruned
	// rather than guessing at the default.
	Keep  int    `json:"keep"`
	Error string `json:"error,omitempty"`
}

// Activation is the result of pointing bin/ at a build.
type Activation struct {
	OK     bool   `json:"ok"`
	Active string `json:"active"`
	// Previous is what was current before, so an operator who rolled back by
	// accident can roll forward without listing again.
	Previous string `json:"previous"`
	Error    string `json:"error,omitempty"`
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
