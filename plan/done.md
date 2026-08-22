# corrallm — completed work

Archive of finished trees. Active work and conventions live in `plan/plan.md`;
deferred, opt-in next-steps live in `plan/icebox.md`.

## ✅ P25 — toolchain registry (COMPLETE 2026-08-21)


Design doc: **`plan/p25-toolchain.md`**. Scoped 2026-08-17; **P25a shipped the
same day** — registry + recipes + agent surface + CLI, read-only plus
`install-deps`. All phases shipped.

corrallm knows what models it runs and nothing about the **programs that run
them** — llama.cpp is a path in a `cmd:` string and that is the whole of its
awareness. Three forcing functions: the tools move (llama.cpp ships daily;
LM Studio and Unsloth pushed tool-calling changes that need a fresh build);
there is a second engine worth tracking ([ninfer](https://github.com/Neroued/ninfer),
OpenAI-compatible, **sm_120a only**); and the same binary is spelled as two
hand-maintained absolute paths across box1 and carlsmacbookpro, so nothing links
a rebuild to the models depending on it.

Decided (user, 2026-08-17): recipes are **bash in corrallm's tree**
(`scripts/tools/<name>.sh`, ported from ml-kit's `llama-rebuild`, embedded via
`go:embed` so they ride agent self-update); `${tool:name}` cmd binding is
**opt-in per model**; builds are **operator-triggered**, with the scheduled
upstream check **on** by default and scheduled rebuild **off**.

A fifth verb landed with it at the user's ask: **`install-deps`**, which installs
what `preflight` reported missing. Doubly gated — the agent refuses it without
`--allow-install-deps`, and the registry refuses it outright on an ADOPTED entry
— and never scheduled. Both refusals return the exact command, so "no" costs a
copy-paste rather than an investigation.

**P25c (2026-08-17)** — `build` shipped for llama.cpp and run for real on box1.
box1's `llama.cpp` is now MANAGED (built into `~/.corrallm/tools/llama.cpp`);
the ml-kit install its models actually spawn is declared beside it as
`llama.cpp-mlkit`, adopted, so the in-use binary keeps its version and drift
visible during the transition. **Two box1 environment faults that only a real
build could surface:** (1) `cc` is gcc 13.3 while `c++` is clang 18.1.3, and
ggml derives its C warning flags from one compiler id — clang's flags reached
gcc and killed the build at 2%; ml-kit pins `CC`/`CXX=clang` and dropping that
was the entire failure. (2) `git remote get-url` applies this box's
`insteadOf` rewrite, so `align_tree` "updated" the origin to the value it
already had on every run; compare `git config --get remote.origin.url`.

**built for real** `0.1.1-dev (build 10472, commit 60eeeb608)` in 481s, both
`sm_86` and `sm_120a` cubins confirmed in `libggml-cuda.so` (one binary for the
5090 AND the 3080), `--list-devices` sees both cards. Stamp verified both ways:
rebuilt when master moved, skipped in 2.1s when it had not. ccache makes
rebuilds ~5x cheaper than the first.
**⚠ operational rule this phase established (USER should know)** a new top-level
config field is NOT safe to add while an OLDER daemon is running. Reading is
fine — `yaml.Unmarshal` ignores unknown fields, and the pre-`tools:` binary
validated the file. But `forWriting` marshals the in-memory struct back out, so
a field the running binary lacks has nowhere to live: at 13:01 the daemon
rewrote config.yml and the whole `tools:` block silently VANISHED. Restored, but
it will go again on the next autonomous config write until a daemon that knows
`tools:` is the one running. **Deploy + restart before relying on it.**
**resolved** deployed + restarted 2026-08-17 (`8e21890`, 0 in-flight, 4 models
evicted and reloaded); `TestToolsSurviveSave` now pins the round trip. Related:
the managed config cannot keep `#` comments either, so everything worth knowing
about a tool moved into its `notes:` field, which does survive a rewrite.

**P25e (2026-08-17)** — `${tool:x}` binding shipped and PROVEN live.
`nomic-embed-text` now reads `${tool:llama.cpp}/llama-server`; SIGHUP reloaded it
with nothing evicted, and it served a 768-dim embedding in 1.6s from
`/proc/<pid>/exe` = `~/.corrallm/tools/llama.cpp/bin/llama-server` — corrallm's
own build — on the **3080**, which exercises the sm_86 half of the multi-arch
build. The other five local models keep their absolute ml-kit paths; opt-in is
the point. Resolution refuses rather than falling back to PATH.
**all six local models migrated** — no absolute ml-kit path remains in any model
cmd. The two hosts migrate to different things, which is the design working: on
box1 `${tool:llama.cpp}` is the MANAGED build (10380 → 10478, a real version
change), on the Mac it is ADOPTED and resolves to the identical path already in
use (a no-op). Each box1 model was load-verified — all four spawn with
`/proc/<pid>/exe` = corrallm's build — and `Qwen3.8-27B` (the one most likely to
break on a version bump, `--spec-type draft-mtp`) was made to GENERATE, not just
pass a health check. Added `corrallm tools resolve` so a migration is checkable
without spawning anything, which is how the Mac was verified without pulling 35B
of weights onto a laptop.
**known gap (P25e)** `SpecFor` derives a MANAGED prefix from the PRIMARY's home
for every host — right for box1, wrong for a remote managed install under
`/Users`. Resolution asks the host instead, so it is host-truthful anyway, but
fix the prefix before managing a tool anywhere but box1.

**P25f (2026-08-21)** — the scheduled drift check, and the last phase.
`toolchain.Watcher` wakes once a minute, and each tool comes due on its OWN
`check:` cadence (default 6h); `check: off` means the host is never contacted at
all, not contacted-and-discarded. Wired in `main.go` beside `watchReload`, and it
re-reads config every pass, so changing `check:`/`rebuild:` takes effect without
a restart. CHECK IS ON, BUILD IS OPT-IN: drift always logs at WARN, and a rebuild
happens only with `rebuild: true` on that tool.
**never rebuilt on a schedule** an ADOPTED entry, even with `rebuild: true`.
`Registry.Build` already refuses one; the watcher refusing FIRST is what stops a
scheduled check filing a guaranteed-to-fail build every six hours forever.
**verified live (box1, 2026-08-21)** one pass against the real config found real
drift on three tools — `llama.cpp` box1 `9731ad3f`→`9a286ac9`, `llama.cpp-mlkit`
`0b1bad14f`→`9a286ac9`, `ninfer` `a05746aa`→`feaf4dd0` — all `rebuild=false`, so
nothing built. The Mac logged "declared but not installed" at INFO rather than as
drift. A second pass immediately after was suppressed by the cadence, as designed.
**caught by mutation testing, not review** the first version of
`TestDriftWithoutRebuildReportsOnly` sampled the call log straight after
`CheckDue` and passed with the `if !t.Rebuild` guard DELETED — `Builder.Start`
returns before its goroutine runs. It now waits on a channel. Every other guard
(cadence, `check: off`, adopted, not-behind) was mutation-checked too; the
unreachable-host case survives single mutations because `st.Error != ""` and
`st.Drift == nil` are redundant for it, and fails only when both are removed.
**decided** absence is NOT drift: a managed tool that is declared but not
installed is never auto-installed by the timer. `rebuild:` is documented as
"build when it finds DRIFT", and first install stays a human action.

**risks** a build competes with resident models for the same GPU (P25c decides
whether it takes an admission slot; starting "reported, not scheduled");
`git clean -xdf` is destructive, so managed trees live under `~/.corrallm/tools/`
and never point at a human's checkout.
**verified live** box1's adopted ml-kit llama.cpp reports `10380 (0b1bad14f)`
from the binary and **BEHIND `34af94cd9`** — drift on an install corrallm never
built, which is the day-one payoff. carlsmacbookpro was really dialled and
answered 404, rendering as the designed "its agent is too old" rather than a
mystery HTTP error.
**found while building it (box1)** TWO nvcc installs: `/usr/bin/nvcc` is the
distro's CUDA **12.0** and shadows `/usr/local/cuda-13.3/bin/nvcc` in PATH.
Trusting `command -v nvcc` reported "CUDA 12.0, too old for ninfer" on a box with
13.3 working. The recipe now uses ml-kit's resolution order and reports the
shadowing as a note even when the answer is fine.
**assumption recorded** the live config was NOT touched — P25a was verified
against a copy. Adding the `tools:` block to `~/.corrallm/config.yml` is the
user's call (below).
**pre-existing, not mine** `TestLiveConfigFreeLaneIncludesThePool` already fails
at HEAD (commit `c0b98eb` repointed groq-llama-70b in the live config without
updating the test); plus four gofmt-dirty files and three vet copylocks hits from
`Config`'s mutex. None touched by P25a.
**measured, not assumed (box1, 2026-08-17)** `llama-server --version` writes to
**stderr** (a naive `$(...)` capture reports "unknown" on a good binary); ninfer
has **no `--version` anywhere**, so a ninfer built outside corrallm is
unidentifiable; ninfer **will not build on box1 today** — CUDA 13.3 ✓, cmake
3.28.3 ✓ (exactly its floor), ffmpeg dev libs ✗ — which is why the recipe
contract has a `preflight` verb.
**trap (load-bearing)** builds must not go through the agent's backend table:
`ReconcileAgent` reaps unclaimed backends after a 60s grace, so a 15-minute CUDA
compile would be killed every time. Separate `/agent/v1/tools/*` surface, and
**Protocol stays at 1** — bumping it takes the whole fleet out until every agent
self-updates, whereas an old agent simply 404s a new route.
**blocking decisions (USER)** whether the live `~/.corrallm/config.yml` gets the
`tools:` block (additive, inert, validates — but it is a running service's
config); whether the llama.cpp pin moves into `tools:` or stays in ml-kit's
`llama.cpp.pin` (P25a adopts rather than owns, so it can wait until P25c); and
whether ninfer is worth running before its checkpoints are on the box.
**optional extensions** other tools (oidio, whichever engine lands next) are the
same shape once the contract exists; not in scope.

