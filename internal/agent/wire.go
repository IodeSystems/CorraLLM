// Package agent is the machine-side half of a multi-host corrallm: a small HTTP
// server that spawns and supervises backends on behalf of a primary daemon.
//
// It is deliberately thin. Everything it does to a process it does through
// internal/host — the same code path the primary uses locally — so there is one
// implementation of "start a backend in its own process group and watch it",
// not two that drift.
//
// What it does NOT do is decide anything. Slot tuning, residency, eviction
// policy and admission all stay on the primary, because they need the tune
// cache, the fair-share ledger and a view of every host. The agent takes an
// already-tuned command string and runs it.
//
// SECURITY: this endpoint executes arbitrary shell commands. That is its
// purpose, not an oversight — the primary sends `sh -c` strings and the agent
// runs them. It is therefore a remote-code-execution surface by design, and a
// token is required unless one is explicitly waived. Treat exposing it exactly
// as you would treat exposing a shell.
package agent

// Protocol is the agent wire version. The primary sends it on every request and
// the agent rejects a mismatch, because the alternative — a primary spawning
// something an older agent cannot describe or reap — leaves a process running
// that nobody can account for.
const Protocol = 1

// ProtocolHeader carries Protocol on every request.
const ProtocolHeader = "X-Corrallm-Agent-Protocol"

// Hello identifies the machine. The primary uses it to confirm it is talking to
// an agent it understands, and to label the host in the dashboard.
type Hello struct {
	Protocol int    `json:"protocol"`
	Version  string `json:"version"`
	Hostname string `json:"hostname"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	// Booted is when this agent process started, unix millis. A change means
	// the agent restarted and anything it was supervising is gone.
	Booted int64 `json:"booted"`
}

// DeviceMem is one memory device's measurement.
type DeviceMem struct {
	Name       string `json:"name"`
	TotalBytes int64  `json:"totalBytes"`
	UsedBytes  int64  `json:"usedBytes"`
	FreeBytes  int64  `json:"freeBytes"`
}

// Capacity is what this machine can actually offer, measured.
//
// Errors are reported rather than zeroed. A zero that means "could not measure"
// is indistinguishable from a zero that means "nothing left", and the primary
// treats capacity as a declared budget anyway — this only auto-fills and
// drift-guards, so an honest absence is strictly better than a confident lie.
type Capacity struct {
	GPU      *DeviceMem `json:"gpu,omitempty"`
	GPUError string     `json:"gpuError,omitempty"`
	Host     *DeviceMem `json:"host,omitempty"`
	HostError string    `json:"hostError,omitempty"`
	// PerProcess reports whether this machine can attribute memory to a single
	// process group. False on macOS, which has no nvidia-smi equivalent.
	//
	// The primary needs to be TOLD this rather than discover it, because the
	// consequence is silent: with no per-process measurement a model never gets
	// a tuning profile, so "reserve the whole pool for one spawn, then measure"
	// never reaches the measuring part and the host quietly becomes a
	// one-model-at-a-time box.
	PerProcess bool `json:"perProcess"`
}

// StartRequest asks the agent to run one backend.
type StartRequest struct {
	// Key is the primary's process identity for this backend. The agent stores
	// it verbatim and hands it back in List, which is what lets a reconnecting
	// primary match what it believes is running against what actually is.
	Key string `json:"key"`
	// Model is for diagnostics only.
	Model string `json:"model"`
	// Cmd is the shell string, ALREADY tuned by the primary.
	Cmd string `json:"cmd"`
}

// Backend is one supervised process group as the agent sees it.
type Backend struct {
	ID      string `json:"id"`
	Key     string `json:"key"`
	Model   string `json:"model"`
	Cmd     string `json:"cmd"`
	Alive   bool   `json:"alive"`
	Exited  bool   `json:"exited"`
	Err     string `json:"err,omitempty"`
	Started int64  `json:"started"` // unix millis
	// MemoryMiB is the group's device memory, or -1 when this machine cannot
	// attribute memory per process (macOS has no nvidia-smi equivalent).
	MemoryMiB int `json:"memoryMiB"`
	// LastSeq is the highest log sequence number available, so a reconnecting
	// primary knows how far behind it is.
	LastSeq int64 `json:"lastSeq"`
}

// SignalRequest asks for a stop.
type SignalRequest struct {
	// Sig is "term" or "kill".
	Sig string `json:"sig"`
}

// LogLine is one captured output line with its sequence number.
//
// The sequence number is the whole point of this being more than a byte stream.
// llama.cpp prints its context size, slot count and KV size in the first couple
// of seconds; the primary parses those to build a model's tuning profile. If a
// control-plane blip loses that window, the model has no profile and cannot
// tune until it has been spawned twice more. `from` lets a reconnecting primary
// replay exactly what it missed.
type LogLine struct {
	Seq  int64  `json:"seq"`
	Line string `json:"line"`
}

// Heartbeat is an agent reporting in to its primary.
//
// The agent initiates this, not the primary. The agent is the side that knows
// it is alive, may sit behind NAT, and may move between networks — so requiring
// the primary to reach IN for liveness would make the common laptop topology
// look permanently down.
type Heartbeat struct {
	// Server is the `servers:` key this agent believes it backs. The primary
	// matches it against config, so an agent pointed at the wrong name is
	// rejected rather than silently accepted as some other host.
	Server string `json:"server"`
	Hello  Hello  `json:"hello"`
	// Backends is what the agent currently supervises, so the primary can
	// reconcile against what it believes is resident without a second call.
	Backends []Backend `json:"backends"`
}

// HeartbeatAck is the primary's reply.
type HeartbeatAck struct {
	OK bool `json:"ok"`
	// Version is what the primary is running. An agent compares it against its
	// own and updates itself when they differ AND it is idle — which is what
	// makes iterating on agent code possible without walking to every machine.
	Version string `json:"version"`
	// UpdateURL is where this agent's platform binary lives on the primary.
	// Sent by the primary so the agent needs no knowledge of the layout.
	UpdateURL string `json:"updateUrl"`
	// IntervalSeconds is how often the primary wants to hear from this agent.
	// Sent so the cadence is the primary's decision and can change without
	// redeploying every agent.
	IntervalSeconds int `json:"intervalSeconds"`
}

// EnrollRequest is a machine asking to attach itself.
//
// It carries what the primary would otherwise have to be told by hand: what the
// machine is, and how much memory it actually has. Sizing a server from the
// agent's own probe is the difference between "run this command" and "run this
// command, then go and write a pools block".
type EnrollRequest struct {
	// Server is the name to claim. Ignored when the enrollment token already
	// names one — a token minted for "mac1" may only become mac1.
	Server   string   `json:"server"`
	Hello    Hello    `json:"hello"`
	Capacity Capacity `json:"capacity"`
	// Endpoints are the addresses this agent believes it is reachable at. It
	// knows its own interfaces; the primary knows which of them it can use.
	Endpoints []string `json:"endpoints"`
}

// EnrollResponse hands back the long-lived credential.
type EnrollResponse struct {
	Server string `json:"server"`
	// Token is the per-server agent token, from here on presented on every
	// heartbeat. Shown once; the primary stores it in config.
	Token string `json:"token"`
	// Pools is what the primary decided this server offers, echoed so the agent
	// can log what it was enrolled as.
	Pools map[string]string `json:"pools"`
}
