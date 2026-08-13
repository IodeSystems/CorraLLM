// Package host abstracts WHERE a backend process runs.
//
// Everything corrallm does to a backend after deciding to spawn it — start it,
// watch for its exit, signal it, check the group is really gone, read its
// memory footprint — used to be `*exec.Cmd` plus a handful of syscalls inlined
// in proc.Manager. That is exactly the set of operations that has to cross a
// network when the process lives on another machine, so it is the seam.
//
// Today there is one implementation, Local, and it is the previous code
// verbatim. A remote implementation talking to `corrallm agent` slots in
// beside it without proc.Manager learning anything new.
//
// THE GROUP IS THE UNIT, not the leader. corrallm starts every backend as
// `sh -c "<cmd>"` in its own process group: the leader is the shell, which
// exits as soon as it has exec'd or been signalled, while the llama-server
// grandchild is the process actually holding tens of GB of VRAM. Watching the
// leader is how a "backend exited" log line once coexisted with a live process
// still owning the GPU. Every method here is defined on the group.
package host

import (
	"fmt"
	"io"
)

// Sig is the signalling vocabulary the manager needs. Deliberately not
// syscall.Signal: a remote host receives an intent, not a local signal number.
type Sig int

const (
	// SigTerm asks the group to stop. It is a REQUEST — a backend in CUDA
	// teardown can ignore it for minutes.
	SigTerm Sig = iota
	// SigKill takes it down. Only after a grace period; see proc's evictGrace.
	SigKill
)

// Spec is everything needed to start one backend.
type Spec struct {
	// Name is the served model name, for diagnostics only.
	Name string
	// Key is the PRIMARY's identity for this backend, stored verbatim by a
	// remote host and handed back in its listing. Reconciliation matches on it.
	//
	// It is not the same as Name and must not be defaulted from it. An
	// extension-hosted model is tracked as "extension:<ext>" while its Name is
	// the served model, and a trial is tracked as "trial:<id>" — in both cases a
	// key derived from Name means the agent reports something the primary does
	// not recognise, which reconciliation treats as an orphan and KILLS, while
	// simultaneously deciding the primary's own process has vanished and freeing
	// its pools. Observed doing exactly that to a trial mid-download.
	//
	// Empty falls back to Name, which is correct for an ordinary model.
	Key string
	// Cmd is the shell string to run. It is ALREADY tuned — the slot
	// auto-tuner rewrites --parallel before this point, because tuning needs
	// the tune cache and the local VRAM budget, neither of which belongs on
	// the far side of a host boundary.
	Cmd string
	// Out receives the backend's stdout and stderr, interleaved. The manager
	// tees this to the process's ring buffer, which is where n_ctx, n_slots
	// and the KV size are parsed from llama.cpp's banner — so a host
	// implementation that drops or reorders early output costs that model its
	// tuning profile.
	Out io.Writer
}

// Handle is one running backend process group.
type Handle interface {
	// ID identifies the group for logs. Opaque and host-scoped ("pgid:12345"
	// locally); never do arithmetic on it.
	ID() string

	// Done closes when the GROUP has exited. Err is valid once it is closed.
	Done() <-chan struct{}
	Err() error

	// Signal sends an intent to the whole group.
	Signal(Sig) error

	// Alive reports whether ANY process remains in the group. False only means
	// "nothing left"; an implementation that cannot tell must say so via an
	// error path of its own rather than guessing dead, because a wrong "dead"
	// frees a reservation that a live process is still holding.
	Alive() bool

	// MemoryMiB is the group's device-memory footprint. The error means "cannot
	// attribute", which is a supported state — macOS has no per-process GPU
	// accounting comparable to nvidia-smi — and every caller treats it as
	// "measurement unavailable" and carries on unmodified.
	MemoryMiB() (int, error)

	// MemoryOnMiB is the group's footprint on ONE card, named by its vendor
	// uuid. It exists because MemoryMiB sums every card the group touches, and
	// a model spread across two GPUs then reports a number that is true of the
	// host and false of every individual pool — which is exactly what a
	// per-card ledger must not be told.
	//
	// The error is the same supported "cannot attribute" state as MemoryMiB,
	// and additionally covers hosts that can attribute per PROCESS but not per
	// DEVICE (a remote agent reporting one aggregate). Callers fall back to
	// MemoryMiB and the single-device path, i.e. to the behavior that existed
	// before this method — never to a guess about which card holds what.
	MemoryOnMiB(uuid string) (int, error)
}

// Host starts backends somewhere.
type Host interface {
	// Name is the config `servers:` key this host backs.
	Name() string
	Start(Spec) (Handle, error)
}

// Unavailable is a Host that cannot start anything, used for a server bound to
// an agent before the remote client exists.
//
// It exists because the alternative is catastrophic rather than merely broken.
// If hostFor fell back to Local for an agent-bound server, a model declared to
// run on the Mac would be spawned ON THE PRIMARY — competing for the same GPU
// it was configured to stay off, with a command referencing a binary and model
// files that may not exist here. A loud refusal is the honest outcome, and the
// error names the reason rather than surfacing as a mystery spawn failure.
type Unavailable struct {
	Server string
	Reason string
}

func (u Unavailable) Name() string { return u.Server }

func (u Unavailable) Start(Spec) (Handle, error) {
	return nil, fmt.Errorf("server %q: %s", u.Server, u.Reason)
}
