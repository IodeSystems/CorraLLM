# Icebox — deferred, opt-in next steps

Things deliberately NOT being built now. Each says what it is, why it is parked,
and what would make it worth starting. Nothing here is committed work.

## Free pool: fan out across public IPs, pin for cache

**What.** A free tier is rate-limited per account and often per source IP. With
agents already running on other machines (`internal/agent`, `agentdist`), a
request to the pool could be issued FROM one of those hosts, spreading a
provider's per-IP limits across every box that can reach it — and pinned, so a
given conversation keeps landing on the same egress and keeps its prompt cache
warm.

**Why it is parked.** It needs an egress-selection axis the scheduler does not
have. Today a candidate is (model, credential, placement); this adds "which
host makes the outbound call", and pinning adds an affinity key that has to
survive across requests without pinning so hard that a dead agent strands a
conversation. That is a scheduler change, not a pool change.

**Why the pool makes it tractable.** `extensions.<name>.virtual` is now the one
place that knows a set of models is interchangeable and free — which is exactly
the set worth spreading. The fan-out belongs on that object.

**Resume when.** There is a second machine with a distinct public IP actually
running an agent, AND a provider limit is being hit often enough to measure.
Waits on: systems (a second egress) and evidence (a rate-limit that bites).

**Evidence needed before starting.** Count 429s per provider per day from the
activity log. If the number is small, this is complexity for nothing.

## Toolchain: per-host `rebuild`, and a `tool` entry kind in the dashboard

**What.** Two gaps that P27 (versioned builds, 2026-08-23) surfaced without
closing:

1. **`rebuild:` is tool-level only.** The scheduled drift check builds every
   MANAGED host of a tool whose `rebuild: true`, so making carlsmacbookpro
   managed enlisted a laptop in unattended 10–20 minute compiles. A
   `rebuild:` on `ToolHost` overriding the tool-level flag is a small change
   (`config.Tool`/`ToolHost`, `watch.go`'s report path, validation, one test).
2. **`tools:` cannot be edited from the dashboard at all.** The config entry
   editor knows `server | lane | group | extension` (`EntryKind` in
   `ui/src/EntryEditor.tsx`, `EntryYAML` in `internal/api/configyaml.go`), so
   flipping a host between adopted and managed — the thing that decides whether
   a build is even possible — is an export/edit/`config load` round trip. The
   Tooling panel's rollback UI (list builds, activate one) wants the same op
   surface, so these two land together.

**Why parked.** Neither blocks the capability: the CLI does both
(`corrallm tools builds|activate`, `corrallm config export|load`), and the
scheduled rebuild is a config flag away from being off. This is ergonomics, and
it is a UI + API + codegen slice rather than a one-file change.

**Resume when.** An unattended rebuild on the Mac actually gets in the way, or
the next time somebody needs to change a tool entry and has to reach for the
CLI to do it.

---

## A second Lenny scenario — the consumer handed a key

**What.** A second scenario and walk beside `is-my-box-working`: the developer who was given an
API key and a URL and configured nothing. Same Lenny, different job — the front door, Keys, a key's
own page, Usage, Quota.

**Why it is parked.** Nothing active needs it, and one scenario's fix queue (`plan.md` §6) is
enough work to be going on with.

**What would make it worth starting.** The moment anyone doubts the constant. caselit's lesson:
until two scenarios walk the SAME screens and fail DIFFERENTLY, `ux/lenny.md` might be decoration
and there is no way to tell — its second scenario had the same control refused by two people for
opposite reasons. Also worth it before any claim that a fix "worked": one scenario cannot show
whether the fix or the persona moved.

## Move short-prompt work to the Mac

**What.** Put `local-nomic-embed-text` and the oidio audio models on `carlsmacbookpro` instead of
box1, and route them there.

**Why it might be worth it.** Measured 2026-09-06: the Mac does 71 tok/s prefill against box1's
731, which rules it out for chat (a 60k-token prompt is 14 minutes there). Embeddings and audio
send tens or hundreds of tokens, so prefill speed is irrelevant to them — and every one of those
requests currently competes for box1's SINGLE chat slot, which is the resource actually under
pressure.

**Why it is parked.** Nothing is measurably suffering from it yet: embeddings are a small share of
traffic. It becomes worth doing if the activity log shows embedding or audio requests queueing
behind chat, which is one query away when somebody wants to check.

## Optional extensions — improve the product; nothing active requires them

*(Relocated from `plan.md` §7 during the 2026-08-24 archive pass. Pull in opportunistically.)*

- **Stickiness/affinity weighting** — how strongly a warm backend overrides *ordered list*
  preference (P4 does ttl/evictCost for *eviction*, but the proxy walks strict quality/list order
  regardless of warmth); per-group vs per-request latency hint. Not built.
- **Context-window clamp on degrade** — P7 clamps `max_tokens`; clamping the prompt to a smaller
  backend's context window needs tokenization, so it's deferred (declared `maxTokens` only for now).
- **gRPC surface** — gat gives it cheaply, but no consumer yet; add when one appears.
- **CapacityProbe** (nvidia/drm/amd/metal/none, auto) — declared budget is canonical and
  implemented; the probe only auto-fills undeclared totals, drift-guards, and feeds dashboards.
- **`server.maxConcurrent` host cap** — per-backend slots enforced (P2); the host-wide concurrency
  ceiling parses but isn't enforced yet (layer onto residency).
- **Proactive ttl reaper** — P4 eviction is lazy (on demand); `ttl` only orders victims. A
  background reaper that frees warm-but-expired models for power is not built.
- **Dynamic footprint** — KV scales with slots×context; v1 reserves worst-case `ramUsage`;
  refine with `{base, perSlot}` later.
- **Audio true-duration costing** (post-P9) — P9c costs audio by bytes; cost by actual seconds
  needs duration: parse parakeet `verbose_json`/SRT, or add a local `ffprobe` dependency. Refine
  the byte-basis once P9 is live and the byte→$ error matters.

> **Correction made during the archive pass.** The old §7 footnote claimed "the proactive ttl
> reaper shipped with slot reservations". It did not: `sched.StartReaper`
> (`internal/sched/reservation.go:128`) expires stale **reservations**, not ttl-expired resident
> models. The bullet above stands as unbuilt. Instantaneous queue depth *is* genuinely covered
> by the sampler, and is struck from the list.

## Deferred — out of scope until later

- **NUMA / interconnect** — per-NUMA system pools, PCIe/NVLink cost of multi-GPU splits.
*(Multi-node peer awareness was here; it is no longer deferred — `host.Remote` is the last
step and it is active in `plan.md` §6.)*

## P9f — conversational grace / comfort-fill on contention

*(Moved out of `plan.md` §6 on 2026-08-24. It was listed as a blocking user decision for
months; nothing was ever waiting on it.)*

Depends on P9e (shipped); optionally P9b for TTS-generated fillers. When a **speech-OUT** realtime session can't be admitted immediately
or is preempted, mask the delay instead of stalling/cutting — keyed to corrallm's already-computed
expected delay (Retry-After EWMA + cold-load time): micro (<~300 ms) → nothing; short (~0.3–2 s) →
injected disfluency ("um", "one moment"); long (>~2 s) → spoken "hold on…" + hold music, session
**parked** (not killed) and resumed on free. **Explicit, scoped exception to "transparent
passthrough"** — corrallm *synthesizes/inserts* audio, justified because it's the only layer that
knows the delay. Only applies to conversational (speech-out) sessions, not transcription-only.
Start with **pre-recorded canned clips** (deterministic, no TTS dependency); TTS-generated fillers
later.

**✅ RESOLVED 2026-08-24 — this is not a decision, it is an icebox item, and calling it a
blocker was the error.** Nothing is built, nothing depends on it, no user has asked for it, and
no shipped surface degrades without it. It sat in the decision list for months as though work
were waiting on an answer; nothing was.

It stays parked here rather than moving to `icebox.md` for one reason worth keeping visible: it
is the **only** proposal on record that would have corrallm synthesize and insert content into a
stream it is proxying. Everything else in this codebase is a byte-pipe with accounting. If it is
ever picked up, that is the property being spent, and the trade should be argued on its own
terms — not inherited from a checkbox.

**waits-on:** nobody. Revisit only if a speech-out workload actually appears.

## Tool build settings as config data

**What.** CMake args (and the CUDA toolkit choice) as per-tool config with the recipe as a template,
instead of hard-coded in `internal/toolchain/recipes/llama.cpp.sh`. Owner raised it 2026-10-06 after
P51: "these tool recipes really need to be data, and update able".

**Why it is parked.** The owner chose the library record and the benchmark gate (P52) over it; those
catch a bad build whatever caused it, while config-driven flags only make a fix faster to apply.

**Resume when.** A second flag change is needed in the recipe, or a host needs flags box1 does not.

## CUDA blocking sync instead of spin (llama.cpp patch)

**What.** `cudaSetDeviceFlags(cudaDeviceScheduleBlockingSync)` for non-cc-12.1 GPUs. Measured
2026-10-06: llama-server CPU 100% → 54% under load; speed cost inside run-to-run noise. Patch file
kept at `~/.corrallm/tools/llama.cpp/patches-disabled/0001-cuda-blocking-sync.patch`.

**Why it is parked.** Owner: "TPS and tgs are at a premium, cpu is not."

**Resume when.** CPU wear or heat becomes the constraint; move the file back to `patches/` and the
next build applies it (the P52 benchmark then shows the cost).

## EXL3 (exllamav3 / TabbyAPI) for Swift

**What.** Serve Swift 1.5 as EXL3 through TabbyAPI instead of llama.cpp GGUF. Researched 2026-10-07:
exllamav3 ≥ v0.0.41 supports Qwen3.8's hybrid architecture with MTP drafting, vision, reasoning split
and tool calling (`tool_format: qwen3_coder`; argument deltas do not stream, tabbyAPI #460).

**Why it is parked.** No artifact meets the bar: no KLD regression vs the live Q6_K_S (0.0016,
ukisai, measured). `erlidev/Swift-1.5-Qwen3.8-27B-EXL3` tops out at 5.0 bpw h6, PREDICTED KLD 0.0026
(from `sc_optimize` on the base model, not Swift); 4.5 → 0.0052, 4.0 → 0.0100. 6.0 bpw exists only for
base Qwen3.8 (turboderp). A Swift 6.0 would need our own convert (BF16 ~51 GB) and is ~the same
size as Q6_K_S, so decode — bandwidth-bound — has no reason to be faster; 3090-scaled estimates put
EXL3+MTP at rough parity with llama.cpp+MTP on the 5090, with slower MTP prefill reported (tabby
#451). Owner, 2026-10-07: "People seem to love exl3, but what I'm seeing is, high KLD and almost
indiscriminate quantization traces."

ninfer-ext (its own EXL3 format) is out on the same grounds: its quantizer accepts 5.0 bpw h6
(`--bits 10 --head-bits 12`, half-bit units) but only 4.0 bpw has a tuned kernel path, and its only
published artifact is base Qwen3.8 4.0 bpw, KL 0.0332. Upstream ninfer is NVFP4-only (excluded).

**Resume when.** A ≥6 bpw Swift EXL3 is published with KLD MEASURED on Swift (same method as the
GGUF figure) at ≤ 0.0016, and someone reports 5090 decode with MTP above our live ~100 tok/s at
20–50k context.

