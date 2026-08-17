# P25 — toolchain registry: per-host tool availability, versions, and builds

Status: **P25a shipped** (2026-08-17) — registry, recipes, agent surface and CLI,
read-only plus `install-deps`. P25b–f open. The phase entry lives in
`plan/plan.md` §7.

## 1. What this is

corrallm knows what models it runs and what memory they cost. It knows nothing
about the **programs that run them** — llama.cpp is a path in a `cmd:` string and
that is the whole of its awareness.

Three things make that a problem now:

1. **The tools move under us.** llama.cpp ships several builds a day, and both
   LM Studio and Unsloth have pushed tool-calling changes that only take effect
   on a fresh build. Today the only way to know what a host is actually running
   is to ssh in and run `--version` by hand.
2. **There is a second engine.** [ninfer](https://github.com/Neroued/ninfer) is a
   from-scratch CUDA engine for a closed set of Qwen checkpoints, serving an
   OpenAI-compatible API — i.e. a corrallm backend like any other. It is not a
   drop-in for llama.cpp anywhere: it builds for **sm_120a only**. "Which hosts
   can even have this" becomes a question corrallm has to answer.
3. **Paths are copy-pasted per host.** The live config spells the same binary
   `/home/nthalk/local/src/ml-kit/local/bin/llama.cpp/llama-server` on box1 and
   `/Users/nthalk/local/src/ml-kit/local/bin/llama.cpp/llama-server` on
   carlsmacbookpro. Nothing connects a rebuild to the models that depend on it,
   so nothing catches a model pointed at a stale or missing build.

So: a **registry** of tools, their version **per host**, and the ability to pull
and build a fresh copy on a chosen host — starting with llama.cpp and ninfer.

## 2. Decisions taken (2026-08-17, user)

| Decision | Choice | Consequence |
|---|---|---|
| Recipe home | **bash scripts in corrallm's tree**, ported from ml-kit's `bin/llama-rebuild` | that logic is already correct; Go would re-derive CUDA arch detection and patch handling for nothing |
| Cmd binding | **opt-in per model** — `${tool:llama.cpp}` resolved per host at spawn | existing absolute-path cmds keep working; migrate one model at a time |
| Build trigger | **operator-triggered**, plus optional scheduled check and optional scheduled rebuild | a CUDA build is 10–20 min of pegged GPU; that stays a decision |
| Schedule default | **check on, rebuild off** | drift is always visible; nothing builds unasked |

## 3. What was measured, not assumed (2026-08-17, box1)

These are the facts the design is built on. Re-measure before trusting them; the
point of the registry is that nobody should have to.

- **`llama-server --version` writes to STDERR**, not stdout, and exits 0:
  `version: 10380 (0b1bad14f)` / `built with Clang 18.1.3 for Linux x86_64`.
  A recipe doing `v=$(llama-server --version)` captures the empty string and
  reports "version unknown" on a perfectly good binary. Merge the streams.
- **ninfer has no `--version` at all** — no flag, no version string anywhere in
  `apps/`, `src/` or `include/`. Its version can come only from a stamp written
  at build time. Corollary worth stating up front: **a ninfer built outside
  corrallm is unidentifiable**, and the registry must report it as
  "present, version unknown" rather than inventing one.
- **ninfer's build targets** are `ninfer` (CLI) and `ninfer-serve` (HTTP). The
  latter is what a corrallm model would spawn.
- **ninfer will not build on box1 today.** CUDA 13.3 ✓ (needs ≥13.1), cmake
  3.28.3 ✓ (needs ≥3.28 — exactly at the floor, so a downgrade breaks it),
  **ffmpeg dev libraries ✗ missing** (`pkg-config` finds no `libavformat`,
  `libavcodec`, `libavutil`, `libswscale`). This is the argument for a
  `preflight` verb: discovering a missing dependency ten minutes into a CUDA
  compile is the failure mode to design out.
- **ninfer cannot build on carlsmacbookpro at all** — `CMAKE_CUDA_ARCHITECTURES`
  is hard-pinned to `120a` with a `FATAL_ERROR` on anything else, and the Mac has
  no CUDA. Per-host availability is a real constraint here, not a display field.
- **gpu1 (RTX 3080, sm_86) cannot run ninfer either**, even on box1. Buildable on
  a host ≠ runnable on every card in it.
- **box1 has TWO nvcc, and PATH resolves to the wrong one.** `/usr/bin/nvcc` is
  the distro's **CUDA 12.0** from 2023 and shadows `/usr/local/cuda-13.3/bin/nvcc`.
  A recipe trusting `command -v nvcc` reports 12.0 and fails ninfer's ≥13.1 floor
  on a machine that has 13.3 installed and working — and a wrong "you cannot
  build this" is worse than no answer, because it sends you installing a toolkit
  you already have. ml-kit's `cuda_toolkit_home` already refuses to trust PATH
  for this reason; the recipe now carries the same order (CUDA_HOME →
  CUDA_VERSION → newest `/usr/local/cuda-*` → PATH) and reports the shadowing as
  a note even when the answer is fine.

## 4. The recipe contract

One script per tool, `internal/toolchain/recipes/<name>.sh`, five verbs. Every
verb prints a single JSON object on stdout and human progress on stderr, so the
agent parses one and streams the other.

```
probe        → {present, path, version, commit, source, stamp}
               What is installed RIGHT NOW. `source` is "binary" (the tool told
               us) or "stamp" (we remember building it) or "" (present, unknown).
upstream     → {ref, remoteHead, local, behind}
               What the pin points at vs what upstream has. `git ls-remote`;
               no clone, no network beyond one round trip.
preflight    → {ok, runnable, missing[], packages[], commands[], notes[]}
               Can this host build it, and what is absent. Seconds. Never
               compiles. `runnable` is separate from `ok` — see below.
install-deps → {ok, allowed, ran[], error}
               Installs what preflight found missing. Mutates the system.
build        → {ok, version, stamp, seconds}
               Align → patch → configure → compile → install → stamp. P25c/d;
               the recipes refuse it today rather than half-building.
```

They are separate verbs rather than one "status" because their costs differ by
four orders of magnitude — a fork+exec, one network round trip, a handful of
`command -v`, minutes of package installation, twenty minutes of compile — and
the scheduled path must be able to ask only the cheap question. Timeouts are
per-verb for the same reason: one number is necessarily wrong at one end.

**`runnable` is not `ok`.** Buildable and runnable are different questions with
different answers on the same box: nvcc cross-compiles ninfer for sm_120a on a
host whose GPUs cannot run it, and box1 — a 5090 beside a 3080 — can build it and
can only run it on one of the two cards.

**`install-deps` is doubly gated.** The agent refuses it unless started with
`--allow-install-deps`, and the registry refuses it outright on an *adopted*
entry: adoption's promise is that corrallm does not manage what it does not own,
and installing packages on behalf of someone else's build breaks that. Either
refusal is a well-formed answer carrying the exact command, so being told "no"
costs a copy-paste rather than an investigation. It is never scheduled.

The agent already runs arbitrary shell by design, so the flag is not much of a
security boundary — if passwordless sudo exists, a backend `cmd` could already
use it. What it buys is a promise: corrallm does not touch a machine's system
packages unless somebody enabled it on that machine, deliberately.

### Carried from ml-kit's `llama-rebuild`, deliberately

The stamp is the piece worth porting exactly. ml-kit's `.built-from` records
**`HEAD + patch-set hash + CUDA arch list`**, and each of the three is there
because omitting it produced a wrong skip:

- `git apply` does not move HEAD, so a HEAD-only stamp skips the build after a
  patch edit and you test the old binary believing it is new.
- adding a GPU changes neither HEAD nor the patch set, so without the arch list
  the build is skipped and the binary cannot see the new card.

Also carried: patches are **reverted before aligning** (an applied patch looks
like the human's uncommitted work, and the align guard would refuse to move the
tree), and an align **never clobbers a tree with uncommitted tracked changes**.

### Not carried

ml-kit installs to `local/bin/<name>` under its own root. corrallm installs to
its own home: `~/.corrallm/tools/<name>/{src,bin,.stamp}`. ml-kit keeps its
build pipeline; this is a second, independent consumer of the same upstream —
not a takeover. Nothing in ml-kit changes as part of P25.

## 5. Transport — and the trap that shapes it

**Builds must NOT go through the agent's backend table.** `proc.ReconcileAgent`
reaps any backend an agent reports whose key no primary Process claims, after a
60-second adoption grace. A 15-minute CUDA compile registered as a backend gets
killed just after it starts warming up, every single time, and the reap looks
like a mystery build failure.

Two ways out; taking the second:

1. Register the build in `m.procs` the way `proc.Trial` does. Works, but it puts
   a compile into the residency ledger — a thing with pools, eviction and
   admission — none of which describes a build.
2. **A separate agent surface**: `/agent/v1/tools/*` with its own job table, so
   reconciliation never sees it. A build is not a backend and should not be
   modelled as one.

**Protocol stays at 1.** Bumping it would make every not-yet-updated agent reject
*all* requests from the upgraded primary, taking the fleet out until each one
self-updates. Adding routes costs nothing: an old agent 404s them, and the
primary reports "this host's agent is too old to build" — which is true,
specific, and self-correcting once its heartbeat pulls the new binary.

**Recipes ship by `go:embed`, not a new distribution channel.** The agent is the
same `./cmd/corrallm` binary, cross-compiled (`make agents`), and it already
self-updates on a build-id mismatch. Embedding means recipes version with the
agent and arrive by a path that is already proven. The agent writes them into
its state dir at run time and executes from there.

That is why they live in `internal/toolchain/recipes/` rather than the
`scripts/tools/` this document first proposed: `go:embed` cannot reach outside
its own package directory, and the alternatives (a generated file, a second
embed package pointing up the tree) buy nothing. `recipes` is a leaf package
importing nothing of corrallm's, so `internal/config` can validate that a
declared `recipe:` exists without a cycle.

**The whole set is extracted, not one file.** Every recipe sources `common.sh`
from its own directory, so extracting a single script produces one that cannot
start. Extraction is idempotent and re-run on every call, which keeps a stale
extraction from outliving an agent self-update.

**The AGENT's embedded copy is the one that runs**, not the primary's. Between a
primary upgrade and an agent's self-update the two can differ, and the older
recipe answers. Self-update converges them within a heartbeat or two.

## 6. Config schema

```yaml
tools:
  llama.cpp:
    url: https://github.com/ggml-org/llama.cpp.git
    ref: master                     # the pin (llama.cpp.pin's job, one tool per entry)
    recipe: llama.cpp               # scripts/tools/llama.cpp.sh
    bin: llama-server               # what `probe` asks for a version
    hosts:
      box1: {}                      # managed: corrallm clones + builds
      carlsmacbookpro:
        installedAt: /Users/nthalk/local/src/ml-kit/local/bin/llama.cpp
                                    # adopted: probe only, never built here
    check: 6h                       # upstream drift check (default on)
    rebuild: false                  # scheduled rebuild (default off)

  ninfer:
    url: git@github.com:Neroued/ninfer.git
    ref: main
    recipe: ninfer
    bin: ninfer-serve
    hosts:
      box1: {}                      # sm_120a — the 5090 only; see §3
```

`installedAt` is the migration path and matters more than it looks: on day one
every existing ml-kit build is **adopted** — tracked, version-probed, visible —
without corrallm cloning or compiling anything. Building is a later, separate,
explicitly-chosen step. Track first, build second.

A host absent from `hosts:` is not "unavailable", it is **undeclared**; the UI
distinguishes the two, because "ninfer can never run here" and "nobody has said
yet" are different facts and only one of them is a bug.

## 7. Cmd binding

`${tool:llama.cpp}` expands, at spawn time, to that tool's `bin` directory **on
the host the model is placed on**. So:

```yaml
cmd: |-
  ${tool:llama.cpp}/llama-server -hf unsloth/Qwen3.8-27B-GGUF:Q5_K_M ...
```

replaces the two hand-maintained absolute paths with one line that is correct on
both machines. Unexpanded absolute paths keep working untouched — this is opt-in
per model, and the two forms coexist indefinitely.

**Refuse, don't guess.** A `${tool:x}` that names an undeclared tool, or a tool
not present on the target host, fails the spawn with that reason. Silently
falling back to `PATH` would spawn whatever `llama-server` the host happens to
have, which is exactly the ambiguity this phase exists to remove.

## 8. Phasing

Each is a green, tested commit per plan.md §0.

- **✅ P25a — registry + probe (2026-08-17).** `tools:` schema + validation;
  `internal/toolchain` (types, Local runner, Registry) and its embedded recipes
  for llama.cpp and ninfer answering `probe`/`upstream`/`preflight`/
  `install-deps`; the `/agent/v1/tools/run` surface plus a `ToolRunner` client;
  `corrallm tools list|preflight|install-deps|recipes`; the agent's
  `--allow-install-deps`.

  Verified live against a copy of the production config:

  ```
  TOOL       HOST             VERSION            SOURCE  DRIFT             DETAIL
  llama.cpp  box1             10380 (0b1bad14f)  binary  BEHIND 34af94cd9  adopted: …/ml-kit/local/bin/llama.cpp/llama-server
  llama.cpp  carlsmacbookpro  -                  -       -                 adopted: agent too old for the toolchain surface
  ninfer     box1             absent             -       -                 ~/.corrallm/tools/ninfer/bin/ninfer-serve
  ```

  Three things that proves, beyond "it runs": **drift on an adopted install
  works** — corrallm never built that llama.cpp and can still say it is behind,
  because the binary reports its own short commit and `ls-remote` supplies the
  other side; **the remote path is real** — carlsmacbookpro was actually dialled
  and answered, and the 404 rendered as the designed compatible failure rather
  than a mystery HTTP error; and **an absent managed tool reports the path it
  would occupy**, so "not installed" names somewhere rather than nowhere.

  Deliberately NOT in P25a: no API/GraphQL op and no UI (that is P25b), and the
  heartbeat does not carry tool state. Surveys are primary-driven and asked live
  on every call, which is why the agent holds no copy of `tools:` to drift out of
  agreement with config.
- **P25b — a Tooling surface.** Per-host table: tool, version, source
  (binary/stamp/unknown), upstream drift, declared-but-absent, undeclared. Where
  the scheduled check lands.
- **◐ P25c — build, llama.cpp (2026-08-17).** `build` verb (align → patch →
  configure → compile → install → stamp), `Registry.Build`, `corrallm tools
  build` with a live log. Preflight gates it; adopted entries are refused.

  **Two environment faults on box1 that only a real build could find**, both
  now carried in the recipe:

  1. **`cc` is gcc 13.3 and `c++` is clang 18.1.3.** ggml's
     `cmake/common.cmake` derives its warning flags from ONE compiler id and
     applies the C set to C targets, so clang's flags reached gcc and the build
     died at 2% with `cc: error: unrecognized command-line option
     '-Wunreachable-code-break'` on `ggml.c`, `sha256.c` and friends. ml-kit
     exports `CC`/`CXX=clang` and never sees this; dropping that line was the
     whole failure. The recipe now pins one toolchain for both languages and
     says so in the log, and an explicit `CC`/`CXX` still wins.
  2. **`git remote get-url` applies `insteadOf` rewrites.** This box rewrites
     `https://github.com/` → `git@github.com:`, so the stored https origin read
     back as ssh, `align_tree` decided the remote had changed, and it "updated"
     the origin to the value it already had — on every run, forever. Compare
     `git config --get remote.origin.url` instead.

  **Built for real on box1:** `0.1.1-dev (build 10472, commit 60eeeb608)` in
  **481s**, installed to `~/.corrallm/tools/llama.cpp/bin` (166 MB). Verified
  independently of the recipe's own report: `llama-server --version` agrees, and
  `cuobjdump --list-elf libggml-cuda.so` shows **both `sm_86` and `sm_120a`**
  cubins — the "every card contributes" rule producing one binary that serves the
  5090 and the 3080. `--list-devices` sees both cards.

  Stamp behaviour verified end to end: a re-run after master moved
  (`60eeeb608` → `058df671b`) correctly REBUILT, and an immediate re-run after
  that skipped in **2.1s** with "already current". Rebuilds are far cheaper than
  the first one (101s) because llama.cpp enables **ccache** when it finds it —
  33.9% hit rate on the second pass.

  A build is deliberately NOT scheduled work: it is minutes of full-machine
  compile that replaces a binary models may be spawning. It stays a decision.

  **A THIRD instance of the two-toolchain trap**, worth stating because it will
  bite again: `cuobjdump` from `PATH` is the CUDA 12.0 build and printed NOTHING
  for a 13.3-compiled object — no error, just empty output. On this box, reach
  for `/usr/local/cuda-13.3/bin/*` explicitly for any CUDA tool, not just nvcc.

### ⚠ Adding a top-level config field is NOT safe while an older daemon runs

Landing `tools:` in the live config looked additive and inert, and it was — for
**reading**. `config.Load` uses `yaml.Unmarshal`, which ignores unknown fields,
and the running (08:42, pre-`tools:`) binary validated the new file happily.
That check was necessary and insufficient.

**The daemon also WRITES config.** `forWriting` marshals the in-memory `Config`
back out, and a field the running binary has no struct member for has nowhere to
live — so it is silently dropped. At 13:01 the daemon rewrote `config.yml` and
the entire `tools:` block vanished; `corrallm tools list` went from three rows to
"no tools declared".

The rule this establishes: **a new top-level config field only persists once a
daemon that knows it is the one running.** Add the field, deploy, restart, THEN
write it into config — or accept that it survives only until the next autonomous
config write (a discovery refresh, a UI edit, an enrolment). Nothing warns; the
block just disappears.

**Resolved 2026-08-17:** deployed and restarted (`8e21890`), so the running
daemon now knows `tools:`. `TestToolsSurviveSave` pins the round trip, including
that `installedAt` survives — an adopted entry silently returning as managed
would make somebody else's tree buildable.

**Second-order consequence, also handled.** The live config's own header says it
plainly: *"Hand edits are NOT preserved: this file is rewritten whenever
configuration changes, and a marshaller cannot keep YAML comments."* The
`tools:` block was originally landed as a wall of explanatory comments —
including, with some irony, the warning about this exact class of loss. All of
it has been moved into the `notes:` fields on each tool and host, which ARE part
of the config and do survive. Anything worth knowing about a tool goes in
`notes:`, never a `#` comment.
- **P25d — ninfer recipe.** `preflight` reports box1's missing ffmpeg before
  anything compiles; hard-fail on non-sm_120a hosts with the reason.
- **✅ P25e — `${tool:}` binding (2026-08-17).** Expansion at spawn time against
  the host the model runs on, resolved through `Registry.ExpandTools` and wired
  into `proc.Manager.ExpandCmd` (a function, so proc keeps no dependency on the
  toolchain). Opt-in per model: a cmd with no reference never touches the
  registry.

  **Proven live.** `nomic-embed-text` now reads
  `${tool:llama.cpp}/llama-server`, reloaded with SIGHUP (no restart, nothing
  evicted), and served a 768-dim embedding in 1.6s. The spawned process is
  `/proc/<pid>/exe` → `~/.corrallm/tools/llama.cpp/bin/llama-server` — corrallm's
  own build, holding :5801 — and `nvidia-smi` puts it on the 3080 at 788 MiB,
  which exercises the **sm_86** half of the multi-arch build rather than only the
  5090's sm_120a. The other five local models keep their absolute ml-kit paths,
  which is the point of opt-in.

  **It refuses rather than guessing.** Not built, not declared on this host,
  host unreachable — each fails the load with its reason and the fix. A PATH
  fallback would run whichever llama-server the machine happened to have,
  silently, at load time, where a wrong binary presents as a model bug. proc
  also refuses when no resolver is wired, since a literal `${tool:x}` handed to
  sh expands to nothing and runs whatever the rest of the line names.

  **Known gap this exposed (not fixed here):** `SpecFor` computes a MANAGED
  tool's prefix from the PRIMARY's home for every host — correct for box1, wrong
  for a remote managed install on a machine whose home is `/Users`. Resolution
  sidesteps it by asking the host (probe returns the real path), so the resolver
  is host-truthful even where `SpecFor` is not, but the prefix itself must be
  fixed before any tool is managed anywhere but box1.
- **P25f — scheduled check (on) / rebuild (opt-in).**

## 9. Risks and open items

- **risk** A build competes with resident models for the same GPU and CPU. P25c
  must decide whether a build takes an admission slot or is merely reported;
  starting with "reported, not scheduled" and watching what it costs.
- **risk** `git clean -xdf` in ml-kit's build path is destructive to anything
  untracked in the tree. corrallm building into its OWN `~/.corrallm/tools/<n>/src`
  rather than a human's checkout is what keeps that safe — never point a managed
  entry at a working tree someone edits.
- **risk** cmake 3.28.3 on box1 is exactly ninfer's floor. A distro downgrade
  breaks the build with a message that will not obviously mean "cmake".
- **decided (2026-08-17)** the live config carries `tools:`. box1's `llama.cpp`
  is **managed** (corrallm builds it under `~/.corrallm/tools/llama.cpp`); the
  ml-kit install box1's models actually spawn is declared separately as
  **`llama.cpp-mlkit`**, adopted, so the in-use binary keeps its version and
  drift visible while corrallm's own copy is proven beside it. Retire that entry
  when the models move to `${tool:llama.cpp}` (P25e). The Mac's `llama.cpp` stays
  adopted — a tool can be managed on one host and adopted on another.
- **note** a plain `make deploy` does NOT refresh the agent binaries; only
  `--agents` (or `make agents`) does, and `bin/agents/` was 13 days stale, which
  is why carlsmacbookpro 404s the toolchain route. Self-update also requires that
  agent to be idle, and the Mac hosts a model.
- **open (USER)** whether the llama.cpp **pin** moves into corrallm's `tools:`
  or stays in ml-kit's `llama.cpp.pin`. Two sources of truth is the bad outcome;
  P25a adopts rather than owns, so this can be decided at P25c without rework.
- **note** box1's adopted llama.cpp is **behind master** as of 2026-08-17
  (`10380 (0b1bad14f)` vs `34af94cd9`). That is the LM Studio / Unsloth
  tool-calling motivation showing up as a number, which was the point.
- **found, not fixed (pre-existing, unrelated to P25):**
  `TestLiveConfigFreeLaneIncludesThePool` fails at HEAD — commit `c0b98eb`
  repointed `groq-llama-70b` to gpt-oss-120b in the live config without updating
  the test that asserts on it. Also pre-existing: four `gofmt -l` files
  (`internal/proc/manager.go`, `internal/proxy/inflight.go`,
  `internal/quota/ledger.go`, `internal/sysmem/sysmem.go`) and three `go vet`
  copylocks hits from `Config` carrying a `sync.RWMutex` (`config/write.go:40`,
  `api/configedit.go:53`, `api/enroll.go:300`).
- **open (USER)** whether ninfer is worth running at all before its checkpoints
  are on the box — the registry can track it without a model ever using it.
- **assumption** ninfer's `main` is the right ref; the repo publishes no release
  tags. Recorded so a version pin can correct it.
