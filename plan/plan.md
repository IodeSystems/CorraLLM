# corrallm — design & roadmap

> Corral + LLM. An OpenAI-compatible reverse proxy + model lifecycle manager +
> priority/fairshare scheduler with cost-aware overflow. Successor in spirit to
> llama-swap (clean-room; reuse *patterns* from redline2, not code).

**How this plan works: §0.** Active work: §6. Known gaps: §7. Deployment: §8.
Capacity-parked: §9. Finished trees live in [`plan/done.md`](done.md); deferred opt-in
next-steps in [`plan/icebox.md`](icebox.md); **decisions that need you** in
[`plan/open-questions.md`](open-questions.md).

## Status — 2026-10-06

**In production on `:8111`, fronted at `https://llm.iodesystems.com`, serving from box1** (RTX 5090 +
RTX 3080). The 64 GB MacBook Pro stays enrolled but has served no model since P46.

**What runs — one model per role (P46):**

| Role | Served as | Notes |
|---|---|---|
| chat + vision | `local-Swift-1.5-Qwen3.8-27B-Q6_K_S` | Swift 1.5 Q6_K_S on the 5090, `--reasoning-effort medium`, MTP **n-max 4** (measured optimum, 10-07) with `--spec-draft-sampling probabilistic`, **128k ctx, KV q8_0/q8_0** (10-10: p99 context 67k, max seen 108k in 14 d; KV/slot 5,328 → 4,352 MiB, card 30.6 → 29.2 GiB), `--cache-ram 32768`, `-lv 4`. Aliases `local-Qwen3.8-27B`, `Qwen3.8-27B`, `Qwen3-6-27B-MPT`. Sole `chat` lane member — **no fallback, no warm rollback**. `costEquivalent: anthropic-sonnet`. |
| OCR | `local-chandra-ocr-2` | 3080, loads on demand |
| embedding | `local-nomic-embed-text` | 3080, loads on demand |
| audio | `oidio` extension | stt, stt-diarize, realtime-stt, tts |
| Anthropic proxy | `claude` extension | haiku-* / opus-* / sonnet-* |

Removed in P46: Swift 1.0, Qwen3.8 Stock and both uncensored builds, the Mac's 35B, qwen-image
(no image generation), and the `free` aggregator (groq / openrouter).

**The workload** is one agentic coding caller, `sk-aw4`, reaching Swift through its aliases. First
measured hour on the current setup (P47): decode ~106 tok/s avg, prefill ~1,070 tok/s, cache hit
~79% per request, ~30k context per turn. Request time is ~85% decode and ~80% of generated tokens are
reasoning (P42), which is why effort `medium` (−47% tokens vs `xhigh`) and probabilistic drafting
(+12% decode, P44) were the levers that paid.

**llama.cpp auto-updates (owner's call, not pinned), now GATED (P52):** a new build is kept only if
it passes a start check and a speed benchmark of production on the 5090 against the build it replaces
(≤10% slower; ~1 min of production paused per build). Serving `20261006-191956-abeada33` (CUDA 13.3,
cuBLAS 13.6). Never activate a build older than `1fb7ef3e3` while `--spec-draft-sampling probabilistic`
is in a cmd — it exits on the unknown flag. **2026-10-05/06 regression** (P51): builds linked the
distro's CUDA 12.4 after the OS upgrade; prefill ~1,000 → ~200 for ~34 h until fixed.

**Operating rules learned the hard way:** reload config with `systemctl --user reload corrallm`
(plain `systemctl kill -s HUP` kills the backends — P43); measure VRAM fit under a production pause
BEFORE moving aliases (the P42 Q6_K cutover OOM'd production); a pause answers 429 retry-after-300
until lifted (P41); `ui/dist` is the live web root, so a UI build deploys.

**Shipped before this stretch** (trees in [`done.md`](done.md)): the engine (P0–P8), observability +
control plane, audio surface (P9, backends in oidio P12), PDF conversion (P13), lanes (P14), bench
(P15), pause (P17), token counting (P20), provider credentials (P21a–i), local-as-a-provider (P24),
the toolchain registry with versioned builds (P25/P27/P29), config in SQLite (P26), tickets (P28),
and the question-shaped dashboard (Lenny, 2026-09-04/05). **2026-09-30 → 10-02:** P39 context stats,
P41 pause→429, P42 Swift 1.5 + effort medium, P43 ExecReload, P44 probabilistic MTP, P45 per-model
cost equivalents, P46 housekeeping, P47 speed/cache/heat on Traffic. **10-02 → 10-06:** P48/P49 room
thermal + hourly weather, P50 Home Assistant temperatures (◐), P51 CUDA 12.4 link regression fixed,
P52 library record + benchmark gate on new builds.

**Four decisions need you** — [`open-questions.md`](open-questions.md): agent lease self-reap (§1),
whether an honest wait estimate is wanted (§2), whether aw4 should ride the `chat` lane (§4), and the
calls the remap-out may not guess (§5). None blocks current work.

### Next steps — triaged 2026-10-06

Ordered by value to production. Owner actions first, because nothing else unblocks them.

| # | Step | Who | Why now | Effort |
|---|---|---|---|---|
| ✅ 0 | ~~Tuner blind on new llama.cpp (`KV … size` line hidden at default log level)~~ — Swift cmd now has `-lv 4` (2026-10-07, owner OK); perSlotMiB reads 5,328 again. Log ring holds it: 200 of 500 lines in use under traffic. Other llama-server cmds (chandra, nomic) not changed — nomic has no KV cache; chandra on demand. | — | — | — |
| ✅ 1 | ~~Run `sudo bash /tmp/cpu-boost-off.sh`~~ done 12:07 10-06, persistent via tmpfiles.d; cost −2..−3% (noise-sized), CPU 75 → 56–60 °C | owner | — | — |
| ◐ 1a | **Owner's queue (2026-10-07), in order:** (1) ✅ `--spec-draft-n-max 3 → 4`: **+15% decode** (96.5 → 110.6 avg, 97.4 → 111.8 median, 615 vs 258 req; every context bucket +7..+14%; 0 errors) — kept. **n-max 5 A/B running since 15:42:15 10-07** (baseline = the n-max 4 window). (2) ~~EXL3 5.0 bpw h6~~ — **iceboxed 2026-10-07** (predicted KLD 0.0026 vs live 0.0016; no 6 bpw Swift; speed ≈ parity) → [`icebox.md`](icebox.md). (3) **owner OK'd, runs after the n-max 5 A/B:** pin + daemon-build `4ebdf2c7` (gate benches it vs `abeada33`), then unpin and reactivate `abeada33` whatever it shows — staying old is the owner's call. Old ≈ current → toolchain; old faster → code (bisect candidates: #29856 recurrent-state gather, #29994 KV scatter data race; #30020 re-reserve ruled out at runtime — 0 reserves over 6 requests at -lv 4). Gap evidence: live decode is 7–9% below Oct 3–4 in EVERY context bucket; ruled out boost (boost-on Oct 6 = boost-off), power cap (400 W throughout, Prometheus), context mix. | me | owner's picks | A/B ~1.5 h; rest TBD |
| ✅ 1b | **Rejected patch shipped + gate bugs — resolved 2026-10-08.** Blocking-sync patch stayed in the src tree after it was "disabled" (file moved out of `patches/`); dirty tree was built as-is, stamped `patches=none`, pin ignored → production ran it 10-06 12:21 → 10-07 17:01. Fixes, all deployed (`0a20610`): dirty tree fails the build (`0778f97`); benchmark waits for production to EXIT, not just leave the table (`57a0829`); an install never prunes the build it replaced (`0a20610`, which had deleted clean abeada33 seconds before the gate needed it, 10-07 22:51). Unpinned 10-08 17:5x (the temp pin also made the MacBook retry a cmake-less build every 6 h). **Result of the old-vs-new question, live at n-max 4:** clean 4ebdf2c7 (10-07 17:01 → 10-08 17:50, 3,023 req) vs patched abeada33 (291 req): decode 116.7/107.5/99.6/99.3 vs 115.4/111.7/97.3/88.0 by bucket — within ±4% where samples are real (50k+ n=33). **No build-caused gap**; the earlier 7–9% was most likely workload mix. **next:** the next scheduled check builds master clean and gate-benches it vs 4ebdf2c7 with the fixes in. **Draft stats (-lv 4, 10-08 07:05–17:50):** 66% of drafted tokens accepted, mean accepted length 3.64, by position 86% / 71% / 58% / 48% — why 4 beat 3 and 5 lost. **Known, old:** ~1/day aw4 502 "EOF" while queued behind a running request (12 in 14 d, since 09-29; not keep-alive idle) — uninvestigated. | — | — | — |
| ✅ 1c | **Prompt cache 8 → 32 GiB host RAM (`--cache-ram 32768`, live 10-08 18:27:50, owner OK) — RESULT: <50%-cached requests 24.0% → 8.1%, re-processed prompt tokens 24.0% → 8.8%, avg request time 14.8 → 9.9 s (−33%), same avg prompt; evictions 20.6/h → 10.9/h (still fills; 48 GiB = next step if RAM allows, 49 GB available at 32).** Baseline 24 h at 8 GiB: 502 evictions, 684/2,813 requests <50% cached, 24% of prompt tokens re-processed (each miss = 15–50 s re-prefill of 30–50k tokens). Comparison job reports at 600 requests (~3 h). Also 10-08: master `de7fa0a3` (owner: "good cuda fixes") built by the scheduled check at 18:02 and gated vs 4ebdf2c7 with the new fixes — prefill 2,181 → 2,277 (+4%), decode 111.1 → 110.2 (−1%), both within bench noise (±3%); kept. **Experiments queued after:** driver 595.91 → 595.99 (same branch; owner runs apt + reboot; bench before/after), then 615 branch; `--ubatch-size 1024` (fit check first); GPU clock cap for heat. | me | — | ~3 h |
| ✅ 1d | **Driver 595.91 → 595.99** (owner upgraded + rebooted 10-08 21:14): same build de7fa0a3 live before/after — decode by bucket −2/−4/+2/−4%, mixed, within noise. No gain, no harm. Limits and boost-off survived the reboot. | — | — | — |
| ✅ 1e | **Prefill batch size** (10-09 12:13–12:16, Swift paused, uncached /completion): ub 512 → 2,138 / 2,078 tok/s at 16k / 40k, peak 31,264 MiB with a 64k prompt + 2048² image; ub 1024 → 2,120 / 2,094, peak 31,802 (+538 MiB, no gain); ub 2048 → OOM at load. **Kept 512**; ~2,100 tok/s is this model's prefill ceiling on the 5090. | — | — | — |
| ◐ 1f | **Per-request prefill overhead ≈ 0.55 s = context-checkpoint copies** — owner OK'd option (1) 10-09: `~/.corrallm/tools/llama.cpp/patches/0001-keep-restored-checkpoint.patch` keeps a checkpoint THIS task just restored instead of re-saving it (other same-position checkpoints still superseded); building through the gate; measure live by the <500-new-token bucket (before: 0.78 s / 334 tok/s raw, 10-09 12:4x hour). **Verified 10-09 (owner asked):** duplicate save on 591/1,321 restores (45%), not every request — avg saving ~0.13 s/request, ~0.3 s where it hits. Cause = two independent rules (checkpoint before the batch starting the last user message + restore landing on that boundary); #28302 (Sep 8) added superseding purely to dedupe. Upstream: no issue on it; **open PR #30137** (ref-counted checkpoints, no zero-init; 52–76% less cache-update time) targets the copy cost itself → do NOT build the pinned-memory patch; **open PR #30141** (MTP hidden state not saved in checkpoints — stale draft state after restore; may cost acceptance right after a resume). Our patch will stop applying if #30137 lands → build fails loudly, drop/rebase then. Comment for #30137 drafted for the owner to post (public). Also shipped `d88adf0`: Prefill tiles = fused (cache included, 27.9k tok/s) + raw by new-token bucket; the per-request mean (733) misled. (trace, -lv 4, task 183717: 489 new tokens, "990 ms prompt eval" = restore 79 ms + create ckpt 307 ms + compute 277 ms + create ckpt 263 ms; ~330 MiB each at ~1.1 GB/s into pageable host memory). The first save re-creates the checkpoint just restored (upstream `create_checkpoint` "replace an existing checkpoint at the same n_tokens" — `tools/server/server-context.cpp` ~2550): identical data, pure waste. `-ctxcp`/`-cms` do not remove either. ~20% of all prefill time. **Owner decides:** (1) local patch: skip re-saving the just-restored checkpoint (~0.3 s/req, low risk, gated) + (3) upstream issue; (2) pinned-memory copies (~0.25 s more, bigger patch) only if upstream doesn't move. | owner | — | — | **Live result (13:27–14:01, 156 req, 0 errors):** kept 70/144 restores, 0 re-saves; <500-new-token prefill 0.67 → 0.49 s (−27%); other buckets within noise. **Then 10-09 ~14:5x (owner):** PR #30137 (ref-counted checkpoints, unmerged upstream, head d2d35a3d80) as `patches/0001-pr30137-refcounted-checkpoints.patch` + our patch rebased onto it as `0002-…` (checkpoints become shared_ptr<const>; adopting id_task via const_cast — objects are created non-const). Old patch retired to `patches-retired/` AFTER reverting it from the src tree (else the dirty-tree guard refuses). Gate: 2,308/110.9 → 2,285/110.2 (equal). **Live 14:32–15:09, 152 req, 0 errors:** prefill per request vs patch-only — <500 0.50 → 0.47 s, 500–2k 1.15 → 1.01, 2k–8k 2.29 → 1.94, 8k+ 15.4 → 10.7 (n=9); kept 77/147 restores. **Kept both patches.** Remaining ~0.45 s on tiny requests ≈ the end-of-prompt device→host checkpoint copy (pinned staging buffer would be the next lever; nobody has written it). Artifact: https://claude.ai/artifact/Xjn6hn45sbzfrLaR3J9QFn
| ◐ 1g | **LightOnOCR-3-4B vs chandra-ocr-2 — A/B done 10-09.** Registered `local-LightOnOCR-3-4B` (prithivMLmods Q8_0 + mmproj-bf16, card sampling temp 0.2/top-p 0.9, port 5812, gpu1, not in any lane; 6,215 MiB loaded). olmOCR-bench (local dataset), 10 pages × 5 categories (math skipped: its scorer renders KaTeX in a browser), same 2048-px image to both, each with its makers' prompt (chandra `ocr_layout` + its parse_markdown; LightOn empty prompt), scored by olmocr.bench.tests. **Tables 86.8 / 86.8, multi-column 73.0 / 64.9, tiny text 90.4 / 90.4, old scans 36.1 / 36.1 (LightOn / chandra); 17.1 vs 28.8 s/page, 1,201 vs 1,945 output tok/page; 0 errors, 0 truncations.** Headers/footers 14.3 / 89.3 is a CONVENTION (bench wants them absent; chandra's pipeline drops Page-Header/Footer blocks; LightOn transcribes them behind `---` rules). Harness: scratchpad `ocr_ab.py` (venv `ocrvenv`, chandra_pkg = datalab prompts/output). **Prompt variants tried 10-09 (owner asked):** the instruction "omit running headers, footers, page numbers" as a system message → headers/footers 14.3% (unchanged), others ±1 test; as user text after the image → identical 14.3%. LightOn does not follow instructions (card: trained on an empty prompt), so no corrallm prompt-appender for it. Post-strip not reliable: 35/50 outputs have no `---` marker; no positions in plain markdown. **Blocking decision (owner):** LightOn (faster, equal/better reading, keeps headers/footers) or chandra (only one that drops them)? Then switch the OCR role to LightOn (config), keep chandra registered as rollback. **risk:** 10 pages/category is small. | owner | — | — |
| ◐ 1h | **Swift `--parallel` A/B for aw4 throughput (2026-10-10 21:3x–22:1x, Swift paused, private :5899).** Replay of 3 real aw4 sessions × 10 turns (captured payloads, instruct sampling, 2 s tool delay; script in session scratchpad `par/`). Same 178k new / 805k cached tokens in every run. Natural outputs (~95 tok/turn, prefill-bound): p1 11.24 → p2 11.57 (+3%) → p3 11.92 turns/min (+6%). Forced 512 tok/turn: p1 6.62 → p3 7.90 turns/min (**+19%**), aggregate 56.5 → 67.4 tg/s. **Cost:** per-request decode 110 → 53 tg/s; ~5–25 tg/s while another slot prefills; turn p90 33 → 42 s. Swaps 30 → 3 (in p1 they caused no re-prefill). p3 at `-c 147456`: 31,122 / 32,607 MiB. Prompt-cache entries are 4–14 GiB (up to 32 checkpoints × ~350 MiB); 46 evictions / 4 h at 32 GiB. corrallm Swift scheduler is `maxConcurrent 1, maxQueueDepth 1` — would 429 a third concurrent aw4 session. **DECISION (owner, 10-10 22:0x): stay at --parallel 1** — multi-slot needs context-overflow handling (a full shared pool aborts every running request), not worth the complexity. Applied instead: Swift `maxQueueDepth` 1 → 5, `maxWait` 88 s → 12m (= 5 × p99 service 133 s; 429 chains p99 382 s exceeded agentkit's 5m RetryBudget — aw4-ab asked to set ≥ 12m or -1), `--ctx-checkpoints 8`, `--cache-ram 40960` (live ~22:0x); aw4-ab asked to keep ≥ 2 requests out (1 running + ≥ 1 queued, up to 6 streams). Target: busy 72% → ~100%, 267 → ~372 turns/h. **10-11: `--ctx-checkpoints 8 → 3`** (live 08:58; restarts 10:46, 11:57, 12:46, 13:09 reset the cache). aw4 rose to 20–30 conversations/h from ~22:00 10-10; 12 cache entries of ~3 GiB (7 checkpoints ≈ 1.75 GiB each) → 43 evictions / 4 h, misses on returning conversations 14–33/h, hit 0.92 → 0.81–0.87, slot save 1.1 s avg per conversation switch (8% of wall time, counted as busy). Speed flat across builds (tp/s 1,000–1,270, tg/s 105–115). **10-16 05:58: Swift `maxQueueDepth` 5 → 3** (owner; scheduler only, no backend restart). **10-16 09:19: `--ctx-checkpoints 3 → 8`, `--cache-ram 40960 → 57344`, temporary `--log-prompts-dir ~/.corrallm/var/swift-prompts`.** Fused prefill (cached incl.) 18–22k tp/s 10-09/10 → 9–14k since 10-11 → 3.5k 10-16 05:00; raw steady ~2k. llama-server log: 10–56%/h of requests "forcing full prompt re-processing" — they matched 10–17k cached tokens but every checkpoint sat above the match (3 slots: 2 go to the prompt end each turn, oldest evicted first). Evictions were not the cause (2 since midnight). aw4 (10-16): sessions share only ~400 tokens of prompt; compactions 0–5/h; adapts on 429; reorder-prefix issue i_1m52dzvf39hpa9w9cg59a7 filed. Swift template keeps `reasoning_content` in history by default (preserve_thinking unset). Prompt diff 09:19–09:36 (63 prompts): 53 appends, 6 new, 4 rewrites — (A) aw4 replaces an already-sent tool_response with a `[truncated event_id=…]` header 1–2 turns later (below all 3 old checkpoints → whole conversation re-prefilled); (B) new sessions diverge inside the tool list at ~5k chars. Both sent to aw4-2d. After the restart: forced re-prefills only on new conversations (lcp < 100); fused 33.5k tp/s, cache 0.95 at 09:30. Baseline 09:30–11:10 (ckpt 8; scratchpad `aw4measure.py`): 574 prompts, 12 rewrites (7.2/h), 26 new; fused 13.8k, cache 0.856; forced 83/544, 68 with lcp ≥ 1000 (948k tok). Remaining loss: llama.cpp checkpoints only at user-message starts (+ prompt end); aw4 roles share ~16k tokens INSIDE an 18.5k-token system message (tools render first, then system text, then per-session `## The issue`), so a new session of the role cannot resume. Asked aw4 to move per-session text to the first user message. aw4 landed (not live) agentkit 892bb83 (stub boundary in steps of 24), aw4 665cd21a (role-constant first, tools by name), and is doing shared-first tool order (i_1m52ykc20fhjv749gx5zhs). aw4 rebuild live 12:17 (subject in first user message, shared-first tools). 12:17–14:00 vs 09:30–11:00: forced 16% → 5% (902k → 174k tok); new-session shared prefix p50 27k → 65.5k chars; tools block identical within role (0/31 differ); BUT fused 13.4k → 7.1k, cache 0.85 → 0.72: with a 16k+ shared prefix every slot passed `--slot-prompt-similarity` 0.10, so the server kept the other session's slot (reusing only the shared prefix; tokens past match p50 993 → 5,677) and did not save it (f_keep ≥ 0.5). **14:04: `--slot-prompt-similarity 0`** (always save + load, ~1.2 s/request). Read 14:05–15:30 (cold start): fused 17.7k tp/s, cache 0.897, forced 4% (39k tok), tokens past match p50 801 / p90 9,088, all matches from the conversation's own entry; save+load avg 1.25 s/request. Sent to aw4-2d. Cache load MOVES the entry (`server_prompt_cache::load`), so a new session that borrows another conversation's shared prefix (f_keep ≥ 0.25 guard) erases that conversation's state; since 14:05 found-better loads by f_keep: < 0.75: 36, 0.75–0.9: 11, ≥ 0.9: 268 (own continuations match ~0.99). **10-16 17:0x (owner OK): `patches/0003-copy-on-partial-cache-load.patch`** — f_keep < 0.9 copies (token list + shared checkpoints; state bytes kept) and marks the entry most-recently-used; ≥ 0.9 still moves. Build b13 = upstream 6420cf5dd + 0001–0003, gate 1.00 prefill / 1.01 decode, Swift on it 17:07 (cache reset). Owner asked aw4-2d (via me) to distill its shared prefix: 74,840 chars = tools 54,563 (57 tools; descriptions 19.0k, param descriptions 17.6k, schema 17.9k) + system text 19,387 ("Reading the log" 7.3k). **Read 10-16 21:50 (0003 live 17:07; Swift restarts 17:07, 18:57 = scheduled build ea0c4c2c, patches kept; scratchpad `keepread.py`):** 14:05–17:02 vs 17:07–21:50 — fused 22.6k → 17.9k, cache 0.924 → 0.908, prefill 1.51 → 1.79 s/req, forced 3–4% → 3–5%; 109 entries kept, only 7 of 108 borrowed donors later resumed (159k tok); evictions 0 → 230 (~50/h, from 33 min after restart; entries p50 10 → 22). Kept donors are mostly finished aw4 sessions; keeping them fills the 56 GiB cache. Traffic not equal (new-token p90 4.5k → 5.2k). Owner (10-16): keep 24 h. **24 h read 10-17 21:53** (scratchpad `segread.py`, `keepread.py`, `forced.py`; journal kept only from 10-17 10:54 — `-lv 4` fills the 939 MB user journal in < 24 h): 17:07 → 21:53 (28.8 h, 9,698 req, 6 restarts at scheduled builds) fused 21.8k, cache 0.922, prefill 1.48 s/req vs baseline 22.6k / 0.924 / 1.51; per restart segment fused 19.4k–24.1k, per hour 14.5k–29.9k (baseline hours inside that spread). Journal 10:54–21:53 (10.8 h): 383 borrows, all kept; 20 donors resumed (5%, 455k tok ≈ 25 s prefill/h saved); evictions 548 (~51/h vs 0); entries p50 23–25, max 32; forced 3–6%/h vs 3–4%. **Result: no measurable gain** — the 17:07–21:50 drop was traffic, not 0003; 0003 costs evictions and a patch to rebase. **10-18 06:1x (owner): 0003 retired** → `patches-retired/`; src tree verified = 0001+0002; `--log-prompts-dir` removed from the config. Swift restarted 13:01 10-18 without `--log-prompts-dir` (last prompt file 06:58). Next: delete `~/.corrallm/var/swift-prompts` (1.3 GB, ask). **next (was):** confirm "keeping the cached prompt" lines on borrows and that the donor's next turn resumes; 2 h read of fused / cache / forced vs 14:05–15:30 (17.7k, 0.897, 4%); then remove `--log-prompts-dir` (~45 MB/h; kept through 17:07 to measure 0003). **risks:** 0003 keeps more entries → more evictions at 56 GiB; `size()` counts shared checkpoints once per entry (over-counts, evicts early). Patch-stack failure surfaces only in build history, no push — ntfy-on-failed-build proposed, owner decides. **risks:** the save+load runs even when the same conversation continues; aw4 spec-loop (i_1m533knbq5) skews the mix. **optional:** llama.cpp patch to checkpoint at the lcp of a long cross-prompt match (fixes any client). **risks:** +16 GiB host RAM; entry count at 56 GiB unmeasured. **next:** after ≥ 2 h without a restart (from 13:09 10-11): hit rate, evictions/h, save ms, active TTFT vs the numbers above. Then the 24 h read after aw4 ships ≥2-out (queue still empty: aw4 sends one at a time). **risks:** 3 checkpoints = fewer rollback points → more re-prefill on divergence (unmeasured); a turn longer than 12m in line still 429s. | — | — | — |
| ◐ 1i | **DeepSeek V4.1 Flash as a paid backend, locked down (2026-10-18).** Last 24 h of Swift traffic (8,168 req; 21.6M uncached in, 247.3M cached in, 6.37M out) prices at $7.80 off-peak / $15.60 peak (rates from third-party pages; DeepSeek page not checked). API decode 176–212 tok/s vs Swift 110; no batch discount, only cache hits + off-peak (half price). **Done (7621d79, not deployed — live daemon built 10-13):** `hard: true` limits refuse with 429 + Retry-After instead of the never-empty fallback serving anyway; `usage.prompt_cache_hit_tokens` read as cached tokens. Draft config in session scratchpad `deepseek-draft.yaml` (validates with the new CLI): credential `allow: [sk-aw4]`, $15/day + $300/month hard, 60 req/min. **next:** owner sets `DEEPSEEK_API_KEY`; deploy the new binary; check the model id via `/models`; one real request to confirm the usage fields and the `reasoning_content`-in-history rule for tool loops. **risks:** dashboard provider dialog re-save drops `allow` and `hard` (ui/src/ProviderDialog.tsx:313-330); `/v1` at llm.iodesystems.com accepts any key (`unknownKeys` open) and key names are guessable (`sk-aw4`), so `allow` is only as strong as the key string; **`claude` extension DISABLED 10-18 17:26 (owner)**: it forwarded the owner's Claude subscription OAuth token to any caller (one request ever, life-raglit 09-25); all three globs `disabled: true`, reload only, verified 404. Re-enable only with an `ANTHROPIC_API_KEY` credential + `allow` (glob-allowlist fix edaba40 needs the new binary); budgets charge peak rates (overcount ~2× off-peak). **blocking decisions:** turn on unknown-key refusal (would block `cf23e0…` = 250 req to 10-09 and empty-key callers = 61 req to 10-07); rotate keys to random secrets (needs aw4 + other clients changed); allowlist the `claude` extension. **optional:** dialog fields for allow/hard. | me, owner | paid key on a public endpoint | S |
| ◐ 1j | **Make corrallm presentable (owner, 10-19).** Done: heat/room/weather/Home Assistant/BTU removed (9b7ec4e, −2.1k lines; a stored `thermal:` key now loads with one WARN and is dropped on next save). Add model by kind — Remote API / Local server / Local process (4f61620, fbdb892); remote = `addRemoteModel` (c2d41a5): base URL, model id, stored or new key, allowlist, hard $/day and $/month, per-1M prices → extension + provider + credential + model + commandCosts in one edit. Verified end to end on a sandbox daemon (scratchpad `devhome`, :6502, vite dev :6503): stored YAML correct, non-allowed caller refused 503. Provider edits keep allowlist/limits/extra headers. `corrallm config secret set|list` (21c2139). Copy + nav pass (ac45a23): plain sentence-case headings, explanations in info tooltips, Bench under Advanced. OpenAI→Anthropic Messages shim with prompt caching merged (51142ad; `proxy.api: anthropic`, optional `cacheTTL: 1h`; fake-server tests only). Caller corrallm key no longer forwarded upstream (ea6e4cc). **Live 10-19 20:11 (c2b4fed).** Found on deploy: credential headers were never `${NAME}`-expanded (Candidate.Target, proc EnsureReady, catalog, virtual pools) — every credential-keyed remote sent the literal reference (93aa798, c2b4fed). Fireworks 1017 live as `fireworks-deepseek-v4p1-flash-1017`: `allow: [sk-aw4]`, $5/day + $50/month hard, 30 req/min, $0.30/$0.006/$1.20. Verified: reply ok; repeat of a 3k-token prompt 2,902 cached, $0.000084 vs $0.000924; `dun` refused 503. Scheduled llama.cpp build 20:05 failed: upstream #30666 (SWA checkpoint invalidation, n_prefix erase rule) touched 0001's hunk; #30137 still OPEN. Rebased 0001 (kept `**it` + upstream condition; old copies in scratchpad `0001/0002.pre-30666.patch`), 0002 applies on top unchanged; manual build b2 started 20:24. HA_TOKEN removed. Second polish pass (9b95078): tok/s units, one-line model names, plain warnings, idle-caller filter, Placements, Machines pool-label bug (localServer with several servers) — deployed UI-only 20:3x and checked live. 10-20: config notes stripped (archived `~/.corrallm/notes-archive-20261019.yaml`), model-id autocomplete from the provider's /v1/models (a1a591e), viewers get hashed keys and no payloads/logs/raw config (0b0c657). Remote speeds (eebd65d): Fireworks timing headers or the proxy clock fill tok/s, `activity.speed_source` says which (UI `~` + tooltip); allowlisted upstream headers stored with the payload; verified live on Fireworks streaming + non-streaming. Latest benchmark on Info + compare-with-model on the Bench tab (live, browser-checked). Bench history is keyed by served name: Swift's current name has no run; its history sits under `Qwen3.8-27B` etc. Bench on Fireworks first failed: 33 × refused by the `allow: [sk-aw4]` credential (bench runs under a minted `bench-<hex>` key; nothing reached Fireworks). Fixed (see git log 10-20): the running bench's key and an admin dashboard session pass credential allowlists (spend limits still apply; not under --insecure); refusal is 403 now. Re-run started 07:16, requests 200. Provider usage policy (owner ask 10-20): calendar-day/calendar-month spend windows (falloff "$50/month" allowed ~$100 per calendar month); remote 429/503 → pause (Retry-After, else 1 min / 15 s), Retry-After filled for the caller, spill to the next candidate, `upstream 429` rows; ntfy alerts on refusal streak start/end, hard spend cap, `x-ratelimit-over-limit: yes` (`CORRALLM_ALERT_NTFY` in `~/.corrallm/application.properties`); provider x-ratelimit-* headers shown on Budgets. Fireworks docs: 6,000 RPM account, adaptive per-model TPM, Tier 1 = $50/month account cap (pauses the account), 503 load shedding; batch 50% off via a separate jobs API (not supported); no off-peak; no streaming surcharge. **risks:** Fireworks has not yet sent a real 429/503 or its documented TPM headers — paths tested on fakes only; spill test relies on round-robin order. Deployed 10-20 07:55 (incl. fix: config reload now re-seeds limits — before, an edited cap waited for a restart); Fireworks caps now calendar-day/month (started at $0: the ~$0.15 already spent this month is not counted). ntfy test push 200. Bench Fireworks 1017 (run 20261020-071642, $0.12): chat 89% 32/36 vs Qwen3.8-27B (08-14) 76% 26/34 — Qwen's extra fails are 3 OCR probes Fireworks skipped (no image input) + codex-plan-2-cache. 20 × our own 429: the remote model has 1 slot. Fireworks concurrency 1 → 8 and soft req/minute 30 → 120 (10-20, owner: "what's reasonable"); verified 8 parallel, 0 ms queued. Add model defaults remote models to 8. Fresh install could not add a local process (owner 10-20, GitHub build): no server existed for this machine, and the first model under a missing `local` provider was refused. Fixed: Server list offers "this machine: add <hostname> (pools)" — `addLocalServer` sizes it like enrollment, one pool per GPU bound by UUID, largest = gpu0 (a guess at CUDA0; notes say check); the first local model creates the provider. Verified on an empty sandbox (browser, add → stored → listed). Pushed 10-20 (b2f92d1); not deployed live. Bench run totals (owner 10-20): turns, total/peak context, cache %, processed, generated, thinking tokens + % of output, thinking level (request params, else launch flags, else "model default"), tps (uncached prompt/s), tfps (all prompt incl. cache/s), tgs — from the proxy's own rows (bench- key, model, run id → publish time); on the model Compare (with relative Δ), the Info card, the Bench leaderboard and per-model history. Checked in a sandbox with a copy of the bench rows. Found: llm-bench's own totals for Fireworks runs are ~2–3× the proxy's (GLM: 495 turns / 2.16M prompt vs 149 requests / 757k); local runs match exactly. Cause not found (Fireworks sends usage once; agentkit emits once); the new rollups use the proxy's numbers. Not deployed. **risks:** proxy-clock prefill includes network + provider queue (lower bound); UI only checked on the sandbox, not the live config; the shim has not touched Anthropic for real (no API key). **optional:** shared table/dialog components; split model.tsx (2.8k lines). | me | owner ask | M |
| 2 | P50: create the HA mean-of-six-thermostats helper; I point `roomEntity` at it | owner, then me | Heating saved is decided by the whole house, not the office | 5 min |
| 3 | **Decode is not fully back.** Live tg 91–100 tok/s since the fix (10-06 10:00–14:00) vs 104–110 before 10-05; prefill is back (1,030–1,200 vs ~1,000). Compare per context bucket (prompts ran larger: 28–48k vs 26–34k) before calling it a regression. | me | Only open question on production speed | 30 min |
| 4 | Route `corrallm tools build` (CLI) through the daemon, or refuse it while the daemon is up | me | The CLI skips the start check AND the benchmark; it is the path used all through P51 | ½ day |
| 5 | Prove the rollback path live: `tools bench <tool> <id>` (benchmark an installed build against the active one, no build). First use: feed it the kept CUDA 12.4 build `165418-a7fb71fa` — expect a rollback. Also useful for manual A/Bs. | me | P52's rollback is unit-tested only | ½ day |
| 6 | Benchmark drain wait: up to 15 min of 429s behind a long generation. Option: skip the benchmark (build stays inactive, retry next check) when production is mid-request. | me; owner picks the policy | Bounded outage per build | 2 h |

Not triaged up: build settings as config data (owner raised; not chosen) and the CUDA blocking-sync
patch (declined) — both in [`icebox.md`](icebox.md).

**Open work is §6:** P38 (ephemeral machines), P37 (warning controls), P36 (true costs), P35 (the
corral layer), P31 (disabled), P34 (residency queue), the Lenny leftovers, P21c (budget
granularity), the trial spawn + add-model form, the wait estimate, `host.Remote`, and OSS
follow-ups. Parked on hardware: §9.

**Numbering.** Git history is authoritative for phase numbers. A second, dashboard-flavoured
series was written into the old roadmap under numbers that collide with it (P15a/P15b, P17–P21);
those trees are archived in `done.md` § Dashboard & observability under descriptive names.
**Next free number: P54.**

**Deviations from the original design:** (1) UI served from `--web-root` dir, not `go:embed`
(matches redline2); (2) live events use **SSE**, not WebSocket (server→client only, no
dependency, EventSource auto-reconnects — subBroker fan-out preserved). Store is minimal
(activity log + rollup query); no sqlc.

---

## 0. Working this plan

This file is the single source of truth for status. Keep it honest and current —
update it in the **same commit** as the code it describes.

**Status marks.** ◻ todo · ◐ in progress · ✅ done · ⏸ parked · ❓ blocked.
A slice is ✅ **only** when its functional unit meets the Definition of done below — and a
✅ slice does not stay here: it moves to `done.md` in the same pass.

**Four files.** `plan.md` = current state, active work, conventions ONLY.
`done.md` = the archive of finished trees, kept for evidence and traps, never a to-do list.
`icebox.md` = deferred, opt-in next-steps. `open-questions.md` = decisions that need the USER,
and **only while they are open** — a resolved question is deleted from it and its answer written
into the plan item that needed it. **Phase numbers come from git history** — next free is P39.

**Re-check a question against the box before you ask it.** On 2026-08-24, six of eight standing
"user decisions" turned out to be already answered by the running system, the migrated config, or
a comment in the code. Verify first; a decision list nobody prunes hides the ones that are real.

**A phase is a functional unit.** Each `Pn` is an independently shippable slice: it
compiles, its behavior is tested, and the engine still runs with it landed. Don't
start `Pn+1` until `Pn` is ✅. A phase too big to land at once → split it into
sub-units (still each a green, tested commit), not a half-done checkbox.

**Definition of done (per functional unit) — all must hold before ✅:**
1. `go build ./...`, `go vet ./...`, `go test ./...` green; `gofmt -l` reports nothing.
2. New behavior has tests: a unit test for logic, an integration/e2e test for any
   request-path change. A bug fix lands with the regression test that catches it.
3. UI changes: `bin/gen` re-run, `tsc`/eslint clean, the SDL snapshot committed.
4. This plan updated **in the same pass**: the finished tree moved to `done.md` with its
   commit hash, a one-line pointer left here only if something still depends on it; resolved
   decisions moved to `done.md` § Resolved decisions; new discoveries filed (rules below);
   the §Status block synced. A stale plan is worse than none.

**Committing.** Conventional commits. Scaffolding and implementation are **separate**
commits (`chore: scaffold X` then `feat: X`). Commit each functional unit on its own —
never batch unrelated phases. The plan-doc update rides with its phase's commit or a
trailing `docs(plan):` commit (as the P0–P5 history shows). **Don't push unless asked.**

**Every active slice in §6 carries:** **next** (the next concrete step) · **risks** (what
could go wrong / what is untested) · **blocking decisions** (a call the USER owns — name it,
surface it, don't guess) · **optional extensions** (nice-to-haves explicitly out of scope).
Record assumptions so they are catchable.

**Filing new work as you discover it — put it in exactly one place:**
- Needed for the *current* slice → add a sub-item to it and do it.
- A follow-on something active needs → a new **§6** slice with its own next/risks.
- Improves the product but nothing active requires it → **`icebox.md`**.
- Out of scope until much later → **`icebox.md`** § Deferred.
- A shortcut / known gap in code already shipped → **§7 Known gaps**.
- A decision only the user can make → **`open-questions.md`**, AND named beside the work in §6.
  When it is answered: write the answer + its evidence into the §6 slice, mark that slice
  **decision resolved**, and DELETE the question from `open-questions.md`.

---

## 1. Vision

One control plane that "herds" many LLM backends — local processes it spawns and
remote/paid endpoints it forwards to — behind a single OpenAI-compatible surface.
It decides **who gets served, on which backend, at what quality, and at what cost**,
under contention, per caller identity.

It must support the full set of fairshare semantics (the "farewell post"):

1. **Lane priority** — higher classes move ahead; may preempt lower (interrupt optionality).
2. **Constrained throughput ratio** — under capacity pressure, weave admission by identity weight.
3. **Unconstrained / cost-shaping** — mostly TCO/$ shaping; always emit backoff info.

…across two cost dimensions (**request count** and **time-in-request / request cost**),
plus **load-spreading** (local saturated → spill to remote → spill to paid) and a
fourth saturation exit we surfaced: **service-quality degradation** (serve a smaller/
cheaper variant).

The engine is one pipeline; every flavor above is a *configuration* of it.

---

## 2. Stack (mid-weight reuse of redline2 patterns)

| Layer | Choice | Why |
|---|---|---|
| API | **Huma + gwag/`gat`** | one Go handler → REST + GraphQL (+ gRPC later). The "register once, typed everywhere" loop. |
| UI client | **graphql-codegen + graphql-request**, `gql` tagged templates, graphql-eslint | typed React call sites from the dumped SDL; no hand-written DTOs |
| Frontend | **React 19 + Vite + TanStack Router/Query + MUI** | matches redline2; file-based routes |
| Store | **SQLite / embedded** (config in YAML), metrics = in-mem ring + persisted rollups | a proxy is mostly stateless; no Postgres |
| Codegen | single **`bin/gen`** orchestrator (sdl dump → graphql-codegen → lint) | offline, deterministic |
| Config | YAML primary (llama-swap-style), layered `.properties` for secrets/env | operator-friendly |
| Dev | air (Go) + Vite, proxied; UI `go:embed` in prod binary | single binary ships the UI |

`gat` decode: register a handler with Huma once; `gat` projects it to GraphQL/gRPC,
`dump-graphql` writes a committed SDL snapshot, codegen turns it into a typed TS client.
That handler→typed-call-site loop is the redline2 pattern we're carrying over.

---

## 3. Core concepts

### Served name → model (pinned) or lane (fallback) — schema v2 (P14)
A served name (what clients put in `"model"`) is either a **model** — exactly one
serving path — or a **lane** — a named, ordered fallback list over model names:

```yaml
model:
  cmd?:   string        # spawn it (local; proxy is the port it binds) — XOR standalone proxy
  proxy:  number | "host:port" | { host?, port?, headers? }   # forward target
  type:   string        # cost class: chat | embed | openrouter | … (keys into commandCosts)
  quality: int          # relative quality rank (higher = better)

lanes:
  <name>: { members: [modelName | {model, sticky}] }   # fallback order, best first
```
- `cmd` present → spawn + health-check + proxy to local port; absent → standalone proxy
  model (remote/paid; no residency knobs). `headers` → auth for remote ($) endpoints.
- Same weights via two paths = two named models, composed in a lane.
- **Fall-through** (overflow *and* degrade) = requesting the LANE name; a model name pins.
- **Round-robin within the same `type`** (cost-equivalent); **quality tiers ordered best-first**.
- A lane member's `sticky` overrides the model's own when the lane's request loaded it.

### Cost model — everything resolves to $
```yaml
costPerKwh: 0.14          # configurable; converts local energy → $
costBaseline: anthropic-sonnet  # optional: which paid type the dashboard's
                                # "would have cost" compares against. Unset →
                                # the CHEAPEST configured paid type, so the
                                # claimed saving is a floor (P36).
commandCosts:
  local:  { generateWattsPerToken: 0.9, processWattsPerToken: 0.3 }   # → kWh → $
  claude: { extract: { costFactor: 0.8 } }                            # $ from response usage
```
- **Local** cost = (gen_tokens·genW + prompt_tokens·procW) → kWh × `costPerKwh` → $.
- **Paid** cost = extracted usage × `costFactor` → $.
- **Which kind a row is** is resolved from the served model's type, not stored:
  energy-priced + a model corrallm runs = electricity; a type with per-token rates = a
  provider's bill; energy-priced + a host we do not run = **unattributed**, which is a config
  gap rather than a reading (§7). `internal/api/costkind.go`.
- **Swap/load** cost = load energy → kWh × `costPerKwh` → $ (plus latency, a scheduling input).
  Charged to whoever triggered the load (or amortized across the coalesced batch).
- Two *uses* of cost, kept distinct:
  - **Share currency** (fairshare ordering): default **request-count**; per-group override to `dwell` or `cost`.
  - **Cost/$ ** (TCO limits, budgets, cost-shaping, reporting): always computed via the above.

### priorityGroup — the single policy unit
A key maps to exactly one group. The group bundles all policy:
```yaml
priorityGroups:
  interactive:
    weight: 10                       # share under contention (in share currency)
    shareCurrency?: requests         # optional override of global default
    interruptible: false             # may a higher group preempt it?
    onSaturated:                     # per backend-type stage policy, walked along the list
      local:  { preempt: true, then: fallThrough }      # take a local slot, else move on
      claude: { spill: true, limits: { cost: "$20/hr" } }# may use paid, budget-capped
      default: reject
  batch:
    weight: 1
    interruptible: true
    onSaturated:
      local:  { queue: true }        # wait for local capacity only
      default: reject                # never spends money
    limits: { dwell: "600s/min" }    # group-wide TCO cap (in addition to per-type)
  default:                           # global default lane for unkeyed/unlisted callers
    weight: 1
    onSaturated: { default: reject }

keys: { aw3: interactive, ragtag: batch }
```
`onSaturated` exits (composable, per type): **preempt** (cancel a lower interruptible
group's slot) · **spill/fallThrough** (advance to next backend) · **queue** (wait + Retry-After)
· **reject** (429). Over-budget (a `limits` cap) feeds the *same* sequence — it's just one
more reason a stage fails and we advance/queue/reject.

### Servers, residency & swap — the resource layer
Beneath scheduling sits **residency**: which models are *loaded where*, bounded by host
capacity, with swap cost and stickiness shaping load/evict decisions. Scheduling decides
*who/where*; residency decides *what's warm*. The two interact every request.

Capacity is a **vector over named memory pools** (each GPU's VRAM + system RAM + …), and a
backend draws from several at once (CPU/GPU offload, multi-GPU split, KV in RAM). A backend
*fits* iff for **every** pool `Σ(resident usage) + this ≤ capacity − reserve`.

```yaml
servers:
  box1:
    pools:   { gpu0: 24GB, gpu1: 48GB, system: 128GB }
    reserve: { system: 16GB }                    # headroom for OS + other procs
    maxConcurrent?: 4                            # optional throughput/power cap

models:
  qwen3-coder:
    sticky: { ttl: "5m", evictCost: high }       # keep warm; resist eviction; anti-thrash
    backends:
      - { cmd: "… -ngl 60", server: box1, ramUsage: { gpu0: 16GB, system: 8GB },
          swap: { loadSeconds: 18 }, proxy: 8081, type: local, quality: 100 }
      - { cmd: "… -ts 10,40", server: box1, ramUsage: { gpu0: 10GB, gpu1: 40GB }, proxy: 8082, type: local, quality: 100 }
```
- **Server capacity** = a **vector over named pools** (per-GPU VRAM + `system` RAM + …) →
  which *spawned* backends can co-reside. Fit = per-pool `Σresident + new ≤ capacity − reserve`
  (vector bin-packing). Mutual exclusivity is **emergent** and **multi-dimensional** (two models
  may share `gpu0` but collide on `system`), not hand-declared groups. Eviction is driven by the
  **binding** pool — only free what relieves the constrained dimension. Proxy/remote backends
  consume no local pools.
  - **Capacity is a declared budget, not a live probe** — vendor-neutral, deterministic,
    testable without hardware, and what actually gates admission/eviction. `server.pools` totals
    + `reserve`; each spawned backend declares its `ramUsage` vector; accounting keeps each pool
    within budget. Apple = a `system` slice (unified); CPU-only = just `system`.
  - **Usage is partly dynamic** — a backend's footprint = weights (static) + KV cache (scales
    with `--parallel` slots × context). `ramUsage` declares the **max at configured concurrency**
    (worst-case reservation); refine later with `{base, perSlot}` if needed.
  - **Probing is optional, pluggable, never authoritative.** Per-pool: the `system` pool is
    universally probeable (`/proc/meminfo`, `sysctl`, `GlobalMemoryStatusEx`); GPU pools use a
    `CapacityProbe` provider — `nvidia` (nvidia-smi/NVML) · `drm` (linux sysfs, amdgpu+intel) ·
    `amd` (amd-smi) · `metal` (darwin) · `none`; `capacity.probe: auto` tries in order, falls to
    `none`. Probe only **auto-fills** undeclared totals, **drift-guards** external pressure, and
    feeds dashboards — correctness never depends on it. (Linux DRM fdinfo gives per-process GPU
    memory for amdgpu/intel to refine a backend's declared footprint over time.)
- **Swap cost** per backend = load latency + load energy (measured EWMA, seeded by config;
  energy → $ via `costPerKwh`). Input to two decisions:
  - **swap vs spill**: target model cold + host full → evict+load (swap cost) **or** fall
    through to a warm/remote backend (spill cost $). Weigh swap-$+latency vs spill-$.
  - **eviction**: which resident model to evict — `evictCost`/stickiness + recency weight it
    (the llama-swap `evict_cost` solver analog).
- **Stickiness**: `ttl` keeps a model warm (idle, not evicted) after last use; `evictCost`
  resists eviction; **affinity** — a latency-sensitive group prefers an already-warm backend
  over paying a cold load, even if it's higher in the ordered list. Per-group: interactive
  avoids swaps; batch tolerates them.
- **Model states**: `absent → loading → ready → idle(warm) → evicting`. Requests for a
  *loading* model **coalesce** behind the single in-flight load (no duplicate loads), then admit.
- **Pinned/preload**: a model may be `persistent: true` (always resident, e.g. embeddings) or
  preloaded at boot; pinned models are exempt from eviction and reserve their VRAM.

---

## 4. Request decision pipeline
```
req → resolve served model, caller key → priorityGroup
for backend in model.backends (ordered; rr within a type):
    stage = group.onSaturated[backend.type] or .default
    if group over a `limits` budget for this type → honor stage (advance/queue/reject)
    try admit on backend (fairshare among groups for its slots, by share currency + weight)
        admitted → proxy, meter (dwell, tokens → $), return
    saturated → apply stage:
        preempt: cancel a lower interruptible group's in-flight slot here, admit
        spill/fallThrough: continue to next backend
        queue: hold with Retry-After backoff
        reject: 429 + structured backoff (X-RateLimit-*, Retry-After, JSON hint)
exhausted → backoff per terminal stage
```
**Backoff is always informative** (Retry-After + capacity/inflight/waiting + reason),
the BackpressureError shape we already validated.

---

## 5. Lessons carried from the llama-swap work
- **Resource/UI passthrough must bypass the scheduler.** The model's own web UI (`/upstream/<model>/…`)
  and other non-inference paths get an *untracked* serve once the backend is up — they
  must not consume admission/concurrency. (The gatedPaths lesson — make it structural here.)
- **Dwell-time, not request count, is the honest cost** for mixed workloads — but make it a
  configurable share currency, default request-count, with measured-dwell available.
- **Interactive ≠ streaming.** Identify interactive by browser signal (Sec-Fetch + Origin)
  if/when relevant, but in corrallm the first-class notion is the **priorityGroup**, not "interactive."
- **Clean-room.** Reimplement from these patterns; do not copy llama-swap source.

---

## 6. Active work

Status marks: ◻ todo · ◐ in progress · ✅ done · ⏸ parked · ❓ blocked.
Everything ✅ has moved to [`plan/done.md`](done.md) — this section is only what is
still open. Items parked on hardware rather than on a decision are in §9.

### ◐ P55 — OIDC sign-in, roles and the Access tab — 2026-10-19

Owner: one OIDC provider (org uses Zitadel, https://id.iodesystems.com), editable from the
dashboard; users with roles admin | viewer; admin token stays admin and can be disabled once SSO
works; `/v1` untouched. Built: `/auth/login|callback|logout|whoami` (state + nonce + PKCE S256,
go-oidc, HMAC session 12 h, `session.key`); client secret optional (PKCE-only client sends
client_id + verifier in the body). Config `auth.users [{email, role}]`, `auth.oidc.disabled`,
`auth.adminToken: enabled|disabled` (disabled needs SSO on + an admin user); legacy
`auth.oidc.allowedEmails` read as admins, folded into `auth.users` on the next Access save.
Role resolved per request from live config. Viewer gate in `auth.Middleware` (`internal/auth/role.go`):
GET/HEAD, or a GraphQL document whose operations are all queries (executor's own parser); else 403
`read-only access`. Per-user session cutoff + last sign-in in store table `auth_users_state`.
API (admin only): listAccess, upsertUser, removeUser, revokeUserSessions, setIdentityProvider,
testIdentityProvider, setAdminToken (disable only from an SSO admin session). Recovery CLI:
`corrallm auth admin-token enable|disable|status`. UI: Setup → Access tab; Read-only chip; viewer
buttons disabled on Setup, models, model page, active requests. Config revisions name the SSO user.
Not yet run against Zitadel; live config unchanged.

- **next:** owner creates the Zitadel Web app (auth method PKCE), registers the redirect URI shown
  in Access, adds self as admin, enables SSO, signs in, then disables the admin token.
- **risks:** viewers can read everything a GET serves, including `keys` (caller API keys, usable on
  `/v1`) and `configExport`; untested against a real provider; every SSO request does one SQLite
  lookup for the cutoff; a literal (non-`${REF}`) client secret is dropped by a form save (UI warns).
- **blocking decisions:** should viewers see caller API keys / the config export? (currently yes).
- **optional extensions:** hide secrets-adjacent reads from viewers; groups / domain allowlist;
  several providers (declined for now).

### ◐ P54 — Anthropic Messages shim: prompt caching behind the OpenAI surface — 2026-10-19

Owner: agentic callers on the `claude` extension re-pay full input every turn, because Anthropic's
OpenAI-compatible layer has no prompt caching. Built (`internal/proxy/anthropic.go`): proxy object
key `api: anthropic` (+ optional `cacheTTL: 5m|1h`, validated at load) makes `/v1/chat/completions`
go to `/v1/messages` with the body translated, ≤4 `cache_control` breakpoints (last tool, last
system block, last block of the final two user turns; caller markers honoured and counted), and the
reply (JSON + SSE) translated back with `cached_tokens = cache_read_input_tokens`. Tests: table
tests + an httptest fake Anthropic through the full proxy path (activity row carries cached tokens
and the cached-rate cost). Not yet against the real API; live config unchanged.

- **next:** owner adds `api: anthropic` to the `claude` extension's proxy and checks
  `cached_tokens` > 0 on the second turn of an aw-style conversation.
- **risks:** cache writes priced at 1× input (Anthropic bills 1.25×/2×) — slight undercount, by
  design. `temperature`/`top_p` and forced `tool_choice` are passed through; current Opus/Sonnet 5.5
  reject them with a 400, which reaches the caller as-is. Whitespace-only `stop` entries are dropped
  (assumed Anthropic rejects them). Usage chunk is always streamed, even without
  `stream_options.include_usage`. Live config uses OAuth (`authTokenCommand`) — the shim strips the
  caller's `Authorization` so the token is injected; not verified against the subscription endpoint.
- **blocking decisions:** none.
- **optional extensions:** map `reasoning_effort` → `output_config.effort`; `file` (PDF) parts →
  `document` blocks; a cache-write cost class.

### ✅ P53 — model page summary: quant, KV cache, context, speed and context p90/p99 — 2026-10-07

Owner: the model page must report quant, KV cache quants, ctx length, avg tp/tg, total requests, ctx
per request avg/p90/p99. Built (`547bd0b`, `ee2682d`, deployed): `config.ParseLaunch` reads each
placement's cmd (comments, continuations, exports, `-m` / `--hf-file` / `-hf repo:TAG`, `-ctk/-ctv`,
`-c`) → placement fields `quant, cacheTypeK, cacheTypeV, ctxSize, modelFile, llamaServer`. p99 added to
speed `Dist` and context stats. `ModelSummary` strip above the tabs, window picker (default 24 h).
Live check: Swift Q6_K_S, K q8_0 / V q5_1, 188,000 configured / 188,160 loaded; 24 h: 3,478 requests,
tp 1,040 tok/s, tg 96.1 tok/s, ctx 31,951 / 51,310 / 63,594 tokens. Loaded context was 0 on new
llama.cpp (no `n_ctx =` line) — fixed via `n_ctx_slot × n_slots`.

- **thinking tokens (`0cfc8e3`):** llama-server reports only `completion_tokens`; the proxy counts streamed
  `reasoning_content` events (1 per token; think tags not streamed, so they fall in "answer"), non-stream
  by character share. `activity.reasoning_tokens`, -1 = unmeasured. First live turns: 2,049/2,175,
  655/812, 929/1,184 completion tokens were thinking (~80%, matching P42). The context figure already
  included this turn's thinking; earlier turns' thinking is NOT resent by aw4 (0/200 bodies).
- **risks:** UI not looked at in a browser (data verified through the API only). Quant comes from the
  FILE NAME — a renamed file reports wrong; reading the GGUF header would be exact but needs the host.
  Context-per-request covers turns that named a conversation (all of aw4's do: 3,478 of 3,478).
- `go vet` fails on two pre-existing copylocks (`config/write.go:52`, `api/enroll.go:289`, from
  `2b54d67`) — DoD item 1 is red regardless of this work; not fixed here.

### ✅ P52 — library record + benchmark before a new build is kept — 2026-10-06 → [`done.md`](done.md)

### ✅ P51 — production 2-8x slower after the OS upgrade: builds linked CUDA 12.4 — 2026-10-06 → [`done.md`](done.md)

### ◐ P50 — hourly room (and outdoor) temperatures from Home Assistant — 2026-10-02

Owner has Home Assistant at `https://ha.iodesystems.com` (reachable from box1: `/api/` → 401 without a
token, 88 ms) with an outdoor sensor and per-room sensors. The room temperature was the last manual,
whole-window number; now each hour can take its own room reading and so its own mode.

Built (`985248b`, deployed): `thermal.homeAssistant` {url, token: `${KEY}` reference — validated, never
the token itself, roomEntity, outdoorEntity?}. `weather.FetchHAHourly` (history API, TIME-WEIGHTED hourly
mean, unavailable states contribute nothing, °C converted). `runWeather` caches `ha:<entity>` beside the
Open-Meteo location (31 d first, then 2 d / 30 min). `hourlyThermal`: per hour room (sensor, else roomF)
→ mode; outdoor (house sensor, else Open-Meteo, else outdoorF) → fan factor; net figure, hours split
cooling/heating/neutral, room range. Setup → Room has the Home Assistant fields.

**Caught before deploy:** `runWeather` dereferenced `th.HomeAssistant` with no thermal config (nil) —
any box without thermal settings would have crashed at startup. `TestRawSpinup` (real binary) caught it;
the deploy gate on `go test` exit status held.

- **live (2026-10-03):** owner ran `bin/thermal-ha-setup` (`3713e0f`; token never passed through the
  assistant). HA has six per-room "4000W" thermostats — the radiant panels' controllers, confirming
  electric resistance ($4.10/therm holds) — and BigBlue outdoor sensors. **Model corrected by the owner:**
  the office fan pushes heat INTO THE HOUSE (window closed, no AC) → cooling costs $0 (set), outdoor is
  display only (`sensor.bigblue_temperature`, air temp — not the apparent-temperature one first picked);
  heating is decided by the WHOLE HOUSE, not the office. Room is still the office thermostat (`_2`).
- **next:** owner creates an HA "Combine the state of several sensors" helper (arithmetic mean of the six
  thermostats); then point `roomEntity` at it (API or Setup → Room).
- **risks:** BigBlue sensors have only 17 h of history in 31 d (recorder coverage?) — outdoor display is
  sparse; irrelevant to cost now. UI words hours above 72°F as "cooling" even with no cooling equipment.
- **risks:** the office sensor is right only while the box's heat stays in the office; with whole-home
  circulation (owner wishes for an attic→basement circulator) a whole-home aggregate entity would be the
  right room reading — a different entity, no code change.
- **blocking decisions:** none.

### ✅ P49 — hourly outdoor weather (Open-Meteo); cooling priced hour by hour — 2026-10-02 → [`done.md`](done.md)

### ✅ P48 — the room the box heats: cooling cost / heating saved from room + outdoor °F — 2026-10-02 → [`done.md`](done.md)

### ✅ P47 — Traffic: speed & cache distributions, per-model paid comparison, heat in BTU — 2026-10-02 → [`done.md`](done.md)

### ✅ P46 — housekeeping: one model per role; `free` removed — 2026-10-02 → [`done.md`](done.md)

### ✅ P45 — per-model costEquivalent; production priced as Sonnet 5.5; llama.cpp left unpinned — 2026-10-02 → [`done.md`](done.md)

### ✅ P44 — probabilistic MTP drafting (+12% decode on live aw4 traffic) — 2026-10-02 → [`done.md`](done.md)

### ✅ P43 — P42's three follow-ups: unit ExecReload live, medium benched, n-gram A/B'd on aw4 — 2026-10-01 → [`done.md`](done.md)

### ✅ P42 — Swift 1.5 Q6_K_S at medium effort is production; bench of thinking/speculation levers — 2026-10-01 → [`done.md`](done.md)

### ✅ P41 — a paused model answers 429 retry-after; Pause button restored — 2026-10-01 → [`done.md`](done.md)

### ✅ P40 — Swift 1.5 uncensored arm; EXL3 dropped (needs GCC 15) — 2026-10-01 → [`done.md`](done.md)

### ✅ P39 — context size per turn and per conversation end — 2026-09-30 → [`done.md`](done.md) § P39

### ◐ P38 — a machine that comes and goes is away, not broken — 2026-09-29

The owner, on Now's "carlsmacbookpro is not reporting in … it has never reported in": *"we
should be able to mark this guy as ephemeral."* Two findings: the Mac really was off the network
(`corrallm-mac-agent`: `ssh … No route to host`), and "never" was false — agent last-seen lives in
memory, so after a restart it means "not since corrallm started".

Built: `Server.Ephemeral` (rides configdb's server remainder column — round-trip tested);
`agentStatus` = `away` when ephemeral and not up; `setServerEphemeral` op; a *Mark as coming and
going* toggle on the machine page; Now skips `away`; Fleet shows it muted. "never" → "not since
corrallm started" in the fault line, the machine page, and the status hint.

- **next:** deploy (restart), mark carlsmacbookpro, check Now has no fault for it, commit.
- **risks:** an ephemeral machine that DIED reads the same as one that left (owner chose no
  long-absence fault).
- **blocking decisions:** none.
- **optional extensions:** persist agent last-seen so a restart does not forget it; a fault after a
  long absence (the option the owner declined for now).

### ◐ P37 — every warning can be hidden or tuned; the queue clash gets its wait fix — 2026-09-29

The owner, on a clash with reachable depth 0: *"I don't have the action to increase wait time as
a solution. Also, I'd like a suppress (forever, time (default 1 day)) … We should be allowed to
suppress, or increase tolerance."* The only button was "set the queue limit to 0", disabled below
1 — and a limit of 0 means *inherit*, so it would have written the clash back. The text also
called the wait fix "box-wide"; it is not, `Model.Scheduler.MaxWait` is a per-model override.

Built, deployed 2026-09-29 (`bin/deploy -y`):
- `warning_pref` table (kind, subject → suppress_until, threshold). Operational state, not config.
  Subject = served model for `queue-clash`, `''` for the box-wide kinds.
- API: `warningPrefs` (GET), `suppressWarning` (minutes | forever | lift; default 1 day),
  `setWarningThreshold`, `setModelMaxWait` (`/models/{name}/max-wait`).
- Utilization rows carry `allowedWaitMs` + `waitForDepthMs` (depth × meanService / capacity,
  rounded up to a second). Slow-stretch multiple (was const 3) reads the operator's threshold.
- UI: every Warnings row has *Hide for 1 day ▾* (1 h / 1 day / 1 week / until shown); count kinds
  (turned-away, unanswered, open-callers) take "warn only above N"; slow-stretch takes the
  multiple; the clash row gets *Raise the longest wait to Xs* per model. Hidden warnings list at
  the bottom with *Show again*.

- UI: the footer ("Changed here, still in the tables below") also lists every threshold with
  *Back to the default*. Found in the browser check: a threshold hides its own line, and the
  line's controls leave with it, so without the footer it could only be undone via the API.

Verified live, 24 h window: the clash row for local-Swift-Qwen3.8-27B reads limit 1, wait 15.0 s,
fits 0, and offers only *Raise the longest wait to 20.0 s* (the 60-min window has no clash:
mean 14.6 s fits). Hide 1 hour → count 5→4, footer entry, *Show again* restores it. Turned-away
threshold 5000 → line gone, footer entry, *Back to the default* restores it. Store empty after.

- 2026-09-30, owner: *"we don't need to see this if there are no issues"*. The panel renders
  nothing when no warning shows (and nothing while loading). Its footer lists a setting only while
  it holds back a warning that would show at the defaults (slow-stretch needs `countAtDefault`
  from the server for that). Every setting is listed on **Setup → Warnings**. "Back to the default"
  now names the value: *Reset to 3× (default)* / *Reset: warn on the first one (default)*.

- **next:** the owner presses *Raise the longest wait* (a live config change, not pressed in
  the check); confirm the clash clears on the next utilization read. Then → done.md.
- **risks:** thresholds hide only the panel line — the tables below still show everything
  (intended). The slow-stretch multiple also changes the Turned-away panel's own slow-stretch
  line, since both read the same server field. `waitForDepthMs` uses the window's mean service
  time; a shifting mean can re-open the clash after the fix.
- **blocking decisions:** none.
- **assumptions:** thresholds are box-wide per kind (owner chose this); suppression is per kind +
  model only for queue-clash, the others are sums over the box.
- **optional extensions:** per-model thresholds; a note field on a suppression (why it was hidden).

### ◐ P36 — true costs: electricity, a comparison, and the cache read — 2026-09-25

The owner: *"On the main page we have cost — we should report that it's cost in electricity.
Additionally, if like a comparison cost, and I'd like to know cache reads."* And the
vocabulary, given in the same breath: *"Tokens are always measured in cached, processed, and
generated."*

✅ **Built and verified against the live activity log.** Three things, on the dashboard's
existing rollup — no new store column, no migration, no metering change.

**1. Local cost is named as electricity, with its rate.** `internal/api/costkind.go` resolves
each rollup row's served model to its `commandCosts` type and splits `costUsd` into
`energyUsd` / `paidUsd` / `unattributedUsd`. The Now strip and Traffic's tiles now read
**Electricity** (`$0.5030`, sub-label `3.593 kWh at $0.14/kWh`) beside **Paid APIs**, and
`ModelStats` puts the kind under every per-model figure. A counted `$0` on Paid APIs is kept
rather than hidden: it says no request went to a provider, which a missing tile does not.

**2. A "would have cost on a paid API" panel** (`ui/src/PaidComparison.tsx`), on Now and on
Traffic from one component. Priced from the `commandCosts` table already in config, never a
hardcoded rate list. **The baseline is `config.CostBaseline` (`costBaseline:`), defaulting to
the cheapest configured paid type by generated-token rate** — a floor, not a flattering pick —
and its name is on the panel header. It round-trips through config export/restore, verified.

**3. Cached · processed · generated, everywhere tokens are reported.** `processed =
promptTokens − cachedTokens`, the subtraction the Overview's re-read figure already used. Now,
Traffic, `ModelStats`, `ActivityLog` and the model page's Usage tab. The cache hit rate rides
the cached cell rather than taking a fourteenth column.

**THE SPLIT FOUND A REAL CONFIG GAP, and the number is visible rather than folded away.**
Every remote model except the three anthropic ones carries `type: chat`, which is an
ENERGY-priced type — so `openrouter-*` traffic is billed at this box's watt-hour coefficients
for tokens processed on somebody else's hardware. Verified arithmetic: 13,352 processed ×
0.00005 Wh + 2,155 generated × 0.0013 Wh = 3.469 Wh × $0.14/kWh = **$0.000486, to the cent**.
That is neither electricity nor a bill, so it reports as `unattributed` with a sub-label
naming the fix (declare a paid type). It is $0.00049 of a $0.50 day — small, and the point is
that it is not silently inside the electricity figure.

✅ **CLOSED by P36b below** — and the fix it named ("declare a paid type") was the wrong
one. These models are free, so a paid type would misprice them in the other direction; the
class they needed did not exist yet.

### ✅ P36b — the fourth kind: free, and priced against what it would have cost — 2026-09-26

`unattributed` was carrying two unrelated things: a genuine config gap, and traffic a
provider gives away. **`BillFree`** separates them. The declaration is the model's own
`freeTier:` block, tested only for a remote whose type names no per-token rate, in ONE
predicate — `cost.NoCharge` — read by both the dashboard and the proxy's meter, so a stored
row and the screen reporting it cannot disagree. Rejected: a flag on the cost type (`chat` is
shared by local models and pooled remotes, so it cannot separate them without a migration,
and on a local type it would call this box's own power free); all-zero factors (`typeCost.paid()`
reads them as unpriced and bills LOCAL ENERGY, so the row comes back mispriced and
unattributed all over again); a provider-level flag (a provider serves free and paid models
at once — and `providers.openrouter.directory.filter.free` enrols nothing, it is a browse
default an operator can clear, so billing read off it would be a guess).

**The comparison now PRICES free rows** (`ExcludeOffBox` no longer catches them). It is the
traffic the question is most worth asking about — real work, billed to nobody — and unlike an
unattributed row there is no junk figure to compare against: $0 is the price, not an estimate.
Counted in its own panel figure, because a return earned by this box and one earned by a
provider's generosity are different claims about the machine.

**Money cannot vanish.** The meter lets a PROVIDER-REPORTED cost outrank the rate table, so a
pooled model that quietly goes paid under its own free-tier declaration records real spend.
The energy table is the ceiling of what the phantom can be: at or under it the dollars are
dropped (kept visible in `mispricedUsd`); above it they are real money and the row reverts to
`unattributed`, keeping its figure and its call to action. Unit-tested both ways.

**Verified on the live activity log**, 24 h to 2026-09-26 01:05 — isolated daemon on `:6515`,
own `--home`/`--db`/`--web-root`, config from the read-only `config/export`, activity rows
copied out of production read-only. Production `:8111` was never restarted, reconfigured or
written to, and `ui/dist` was never built into (scratch `--outDir`).

| | before | after |
|---|---|---|
| `openrouter-cohere-north-mini-code` (7 req) | `unattributed` $0.000486, **excluded** from the comparison | `free` **$0**, `mispricedUsd` $0.000486, **priced at $0.0241** |
| `openrouter-nvidia-nemotron-3-super-120b-a12b` (1 req) | `unattributed` $0 | `free` $0, excluded `no-tokens` (it recorded none) |
| Unattributed total | $0.000486 | **$0** — only `local-Qwen3.8-27B` remains, the model the Swift cutover retired |
| `claude-haiku-*` | `paid` | `paid`, unchanged |

**NO CONFIG CHANGE WAS NEEDED** for the live box, and the reason corrects the brief's premise:
these nine models are not selections off the `directory` filter (`model_selection` is empty) —
they are the **virtual pool**, `extensions.free.virtual`, whose `template.freeTier` is copied
onto every contributed model by `VirtualModelFor`. The isolated daemon logged `virtual pool
refreshed virtual=free models=9 members=2 of=2` and classified them free on the next request.

**Also fixed, and it is behaviour-changing:** `modelForSelection` read `pv.Discover.Template`
— the RETIRED key — so a `template:` written under `directory:` was silently ignored, and the
one field that shapes a selected model's cost class only worked if you spelled the key the
deprecation warning tells you to stop using. It now reads `pv.Directory`. Nothing on this box
is affected (no selections exist), but any config that DID carry a directory template starts
having it applied.

**next** — nothing required.

**risks / what is untested** — (1) **The model page cannot open a pooled model at all**:
`/m/openrouter-cohere-north-mini-code` renders "Unknown model", so its new `free` wording is
the one surface not seen rendering. Pre-existing and unrelated to this slice — the page
resolves from a source that excludes the discovered overlay — but it means a free model has no
detail page. (2) A **free AUDIO or IMAGE** backend would have a token-derived phantom ceiling
of $0, so its recorded per-MiB/per-image dollars would read as a real charge and revert to
`unattributed`. None exists; the arithmetic for it was never written. (3) `mispricedUsd`
decays to $0 as pre-fix rows leave the 30-day window, so the sub-label that names it
disappears on its own — correct, and unobserved over that span. (4) **The Unattributed tile is
gated on dollars, not on requests**, so this window's 1,460 requests against the retired
`local-Qwen3.8-27B` — 32% of the traffic — show NO tile, because they recorded $0. The
comparison's exclusion note names them with their count and `ModelStats` shows the row, so they
are not invisible; but the tile that exists to be actionable is silent about the largest
actionable thing in the window. **Blocking decision (the user owns it):** give Unattributed a
request count the way Free tier has one, or leave the dollar gate as P36 chose it.

**assumptions** — that a `freeTier:` block plus a rate-free type is a sound assertion of $0.
The block can carry a `usd` spend cap, which is why the paid test runs FIRST: a backend whose
type names rates is paid whatever else is declared about it.

**Live numbers, 24 h to 2026-09-25 15:55** — a second daemon on `:6513` over the live config
(via the read-only `config/export`) and the day's activity rows copied out read-only.
Production `:8111` was never restarted, reconfigured or written to.

| | |
|---|---|
| Electricity | **$0.5030** = 3.593 kWh at $0.14/kWh |
| Paid APIs | $0 (one `claude-haiku-*` request, no tokens) |
| Unattributed | $0.00049 (8 openrouter requests) |
| Tokens | 47,900,005 cached · 11,185,858 processed · 2,336,045 generated — **81.1%** of prompt from cache |
| On `anthropic-haiku` (the default baseline) | **$27.71** — 55× |
| On `anthropic-sonnet` (`costBaseline:` set) | **$82.90** — 165×, of which **$14.37 is cache reads alone** |

**next** — nothing required. The comparison covers what it says it covers and says so.

**risks / what is untested** — (1) **The comparison's honest scope is smaller than the
headline.** Of 2,616 requests in the day it prices **1,910**; the other 706 are 690 served by
a model no longer in the config (queue-timeout 429s against the name the Swift cutover
retired), 8 off-box, 5 with no tokens, 2 speech, 1 already paid. The panel's note lists every
group with its count — but a reader who takes "$82.90" and stops has taken a figure about 73%
of the window's requests. (2) **Images are excluded and the reason is a gap, not a rounding**:
`imageWhPerImage` is unset, so a generated image costs $0 locally and there is nothing to
compare. Named in the note; nothing on this box generated one in the window, so the exclusion
path for images is **unit-tested only**. (3) A **paid** audio provider would read as paid spend
and still sit outside the comparison — correct, and also unexercised: no paid audio type is
configured. (4) The classification is recomputed per request from the CURRENT config, so
repricing a type moves history — deliberate, and the same property §8's calibration backfill
already relied on, but a model renamed today makes yesterday's rows read `unknown-model`. (5)
`Remote()` is the energy boundary: an enrolled Mac's models carry a `cmd` so they read as this
fleet's electricity, which is right, but **a LAN box we do not manage would read as
unattributed** and none exists here to check.

**assumptions** — that the cheapest paid type is the right default baseline (a floor beats a
flattering number, and the owner can name another); that a cached prompt token is free locally
(the cost model already asserted this — `RequestUSDByClass` returns 0 for it — and the
comparison now makes the consequence visible).

**blocking decisions** — none. **One worth an answer when convenient:** `costBaseline` is unset
on the live box, so it defaults to `anthropic-haiku` and reports 55×. If the honest alternative
to this box is Sonnet, saying `costBaseline: anthropic-sonnet` triples the figure to 165× — and
which of those is true is a fact about the owner's workload, not about corrallm.

**optional extensions** — out of scope: a per-caller comparison on the Callers page (the same
rows exist per key); a cost-over-time chart split by kind; renaming the `WattsPerToken` config
fields to `WhPerToken` (§7 already carries this).

### ◐ P35 — the corral layer: transcode in, remap out — 2026-09-23

The owner: *"we also need a transcode layer into it, so that we can maintain compatibility,
and we need a remapping layer out for the same reason."* The product is the framing he gave
earlier — *"speech, like image generation, has odd, non-standard backends, and we intend to
'corral' them into a standard OpenAI interface"* — so corrallm owns the OpenAI shape and a
backend is whatever it is. Design, both directions, with its evidence:
[`p35-corral-layer.md`](p35-corral-layer.md).

✅ **Phase 1 — transcode in — is built and live-verified.** An STT upload is converted to
16 kHz mono wav when, and only when, the backend has DECLARED which audio formats it reads.
The declaration is `config.ModalitySpec.Formats`, which already existed, is already
probed-beats-declared, and is already on `/v1/models` — audio format is the finer grain under
the `audio` modality `config.IO` names coarsely, not a new concept. An empty list means no
constraint is known, so the layer is invisible against today's config: **oidio ingests every
format an OpenAI client sends** (wav/mp3/m4a/flac/webm and a 48 kHz stereo wav all 200 on the
live `:8111`, 2026-09-23), and normalising for it would be CPU spent to make a request no
better. Placed beside the PDF rewrite and BEFORE admission, following `pdf.go`, with one
reason of its own: the refusals are validation, and a caller whose file was never servable
must not queue first to find out. `X-Corrallm-Transcode-Ms` so a bench does not read ffmpeg
as a slow model.

**Proven against a stub reproducing NeMo-Speech.cpp's `audio_file.cpp:38` and forwarding
what it accepts to the real `oidio-stt`** — so an accepted request returns a real
transcription. mp3/m4a/flac/webm: **400 "not a RIFF/WAVE file" before, 200 with the right
text after, 112–135 ms each.** The same mp3 to a second model pointing at the *same stub*
with nothing declared: still 400, Transcode-Ms 0 — the capability is per backend. A 2-hour
m4a: **413 in 0.205 s**, refused off ffprobe without decoding. ffmpeg off PATH: **415**
naming the format, the accepted list and the missing tool. The live `:8111` was not
touched, restarted or reconfigured.

✅ **Contract 2 of the remap out — `stream=true` on STT — is built and live-verified.**
`stream=true` on `/v1/audio/transcriptions` and `/v1/audio/translations` now returns
`text/event-stream`: OpenAI's `transcript.text.delta`, a `transcript.text.segment` per
diarized segment, `transcript.text.done` with a duration usage block, then `[DONE]`.
oidio's diarize path dispatches before the stream check and never reads the flag, so a
client that set it was handed `application/json` while parsing SSE.

**The latency benefit is a fiction and the code, the manifest and the design all say so.**
Measured against the real `oidio-stt-diarize` through a second corrallm on `:6510`:
**ttfb 0.296 s / total 0.296 s**, three runs, widest gap **0.9 ms**. The first event and
the last leave together, because the body was complete before either existed. What it
buys is one client code path. It is also **lossy — 5,640 bytes of JSON became 442 bytes
of events** — because the event schema has no carrier for `language`, `task`, `words[]`
or `speakers[]`; the manifest says to set `stream=false` to keep them. **Both numbers
moved when contract 1 shipped, and the loss did not: the same request is now 665 bytes
of canonical JSON against 407 bytes of events.** Metering is
unchanged by construction — the writer sits outside `statusCapture`, verified at 200 /
123,388 audio bytes / ttfb 298 ms with oidio's own JSON on the row.
**`/v1/audio/speech` was NOT touched** (§9.1 leaves it open; live `{"stream":true}` there
still returns `audio/mpeg`). The live `:8111` was not restarted or reconfigured.

✅ **And the route now has ONE output contract — §12.10.2, shipped under §9.0b.** The
real stream (`oidio-stt`, the NON-diarize path) was measured first: **ten
`transcript.text.delta` events, one `transcript.text.done`, no usage block, no
terminator.** Two divergences from the synthesised shape, not the one that was filed. The
event types and field names already matched, so nothing was renamed. **The terminator is
appended; the usage block deliberately is NOT** — a real stream measured no duration, and
computing one is what §9.0b(f) already forbade, so `usage` is now an explicitly optional
field of the published contract. A passed-through stream is **completed, never
rewritten**: its bytes go downstream unbuffered and unaltered, and a backend that already
sent `[DONE]` or its own `done` event gets no second one. Live on a second daemon on
`:6510` proxying the already-running oidio — nothing spawned, loaded or evicted:
`oidio-stt` ends `data: [DONE]` with its ten deltas intact, `oidio-stt-diarize` still
synthesises the same 442 bytes. The activity row records oidio's 648 bytes while the
client receives 662, so the terminator is corrallm's and is not metered. **The
capabilities manifest's `streaming` note is now normative** and was rewritten as a
specification — it had carried a third false claim ("no STT backend on this box streams")
of the same family as `speech.wav` and `num_speakers`.

✅ **Contract 1 — the canonical transcription body — is built and live-verified.**
**decision resolved 2026-09-23:** *"Yes, project mission statement is openai
compatible."* So the shape corrallm publishes is OpenAI's, `diarized_json` included
(§9.0c), and §9.3 is removed rather than answered. corrallm no longer forwards a
backend's transcription body: `internal/proxy/sttremap.go` maps oidio's and NeMo's onto
one shape, and the upstream request is normalised to one `response_format` because oidio
silently answers its model default for a format it does not know.

**Measured through a second daemon on `:6510` proxying the already-running oidio** — no
`cmd`, nothing spawned, loaded or evicted, production never restarted. Same 2.56 s file:
`oidio-stt-diarize` `json` **5,640 B → 55 B** (the floor: `{"text":…}` and nothing else,
on every model); `verbose_json` **6,348 B → 665 B** with labels `"A"`, string ids and
words that carry no `end`; **`+include[]=backend` → 6,097 B**, the 512-float voiceprint
back with a `label` beside it; `diarized_json` to `oidio-stt` **400** naming the model;
`known_speaker_names[]` and `include[]=logprobs` **400 in 0.6 ms with no backend call**;
the diarized stream **442 B → 407 B** and its speaker now `"A"` rather than a UUID;
`oidio-stt`'s real stream **662 B, byte for byte unchanged**. `text`/`srt` forwarded.
Metering bills the caller's upload rather than corrallm's added form field, and the
activity row still holds oidio's body.

**TWO RULES WRITTEN, not discovered.** The 27-speaker rollover is bijective base-26 —
`"AA"` after `"Z"` — chosen because it agrees with OpenAI exactly over the range OpenAI
specifies and never gives two speakers one label. The NeMo word→segment join is six
clauses (first segment in document order whose half-open `[start,end)` contains the
word's start; an open-ended segment runs to the next one's start; an unmatched word goes
to the nearest segment starting at or before it, or the first; an un-started word is not
placed; text joins in returned order with one space only where neither side has
whitespace; an empty bucket keeps `""`), plus a 2b for a backend that returns
speaker-bearing words and no segments at all.

✅ **The two-call diarized join — built and live-verified 2026-09-25.** The owner: *"do
the two call variant … we are supposed to be an adapter on these wild West audio systems
into a reliable OpenAI API."* When a backend declares `modalities.audio.diarizationPath`
and the caller asks for speakers, corrallm calls that endpoint too and does the
word→segment join itself (RULE 2), rather than trusting what the backend fused onto its
own words. **It is not a refinement, it is a correctness fix**: NeMo-Speech.cpp's fused
output puts the first **597.6 s — 1,372 words — on a single speaker** at 1440 s, while
its own diarization endpoint answers correctly, and the two agree on 97.1% of words at
720 s but only 62.1% at 1440 s. Reproduced on **CC-BY AMI meeting audio the backend
itself ships**, so nothing here rests on private material. **+13% wall clock, not
+100%**, because the transcription leg stops asking for the diarizer pass whose answer
is discarded (identical words, verified); **two compute buffers are never live at once**
— the second call is issued after the first response is complete and before the
residency ref is dropped. **Opt-in per backend, declared like every other audio fact**;
oidio's path is unchanged and no model name is hardcoded. A failed second call is a
**502 naming the endpoint**, never a quiet fallback to the labels it exists to distrust.
Design, measurements and the overlap tie-break:
[`p35-corral-layer.md`](p35-corral-layer.md) §12.12.

**next** — nothing on this tree. The three unbuilt contracts each need an owner decision
first: `open-questions.md` §5 items 2 (translations do not translate), 5 (`url` on
images) and 6 (`/v1/realtime`'s audio format). **§9.0c's larger half — "ensure that is
the case" across every surface, not just audio — is P35 §13 and is unstarted.** Phase 2
(PCM bounds on `ModalitySpec`) is independent of all of it and could go first.

**risks, contract 1** — (1) **The `diarized_json` refusal is LATE.** Nothing in
corrallm's model record declares that a model diarizes, so the only honest signal is
what the backend returned: the caller gets 400 while the activity row records the
backend's 200 and the GPU time it really spent. Verified live. Fixing it needs a config
declaration — `open-questions.md` §5 item 7, new. (2) ~~**Every NeMo path is untested
against NeMo**~~ — **closed**: RULE 2b on 2026-09-23 (`p35-nemo-live.md` §5.1, seven
files) and RULE 2 on 2026-09-25 (§12.12, 491 segments joined on a 1440 s file). What is
still unrun is a SECOND backend with a separate diarization endpoint; the wire reader
accepts a string speaker and a segment `id` and nothing has sent either. (3) **The
27-speaker rollover has never run on 27 speakers** — no such recording exists here;
unit-tested to 1,000 labels for distinctness. (4) **`json` is now a breaking change for
anyone reading oidio's extras out of it** — that is §12.1's floor working as designed,
and the manifest says so in its first sentence, but a client doing it will notice. (5)
`include[]=logprobs` is a 400 where it was silently ignored. (6) oidio writes
`Speaker 1:` inside an SRT cue; `text`/`srt`/`vtt` pass through unnormalised, documented
rather than rewritten, because editing inside a subtitle cue is editing a transcript.

**risks, contract 2** — (1) **Nothing has been run against a backend that emits real SSE
on the DIARIZE path**, because none exists; the passthrough is covered by a unit test and
by `oidio-stt`, not by a diarizing streamer. (2) ~~one route terminates two ways~~ —
**closed 2026-09-23 by §9.0b, shipped as §12.10.2**: a passed-through stream is now
completed with the `[DONE]` the backend omits, so both halves of the route end the same
way, verified live against `oidio-stt` and `oidio-stt-diarize`. The remaining exposure is
narrow and named: **a real stream that sends deltas and NO `transcript.text.done` gets one
built from those deltas, and no backend on this box does that** — unit coverage only.
(3) A diarize body over 2 MiB would be captured to the activity row as a
tail rather than a head, since `statusCapture.streaming` follows the CALLER's flag; the
sample is 5.6 KB and this has not been hit.

**risks, phase 1** — (1) The declaration is per CONTAINER. A 48 kHz stereo **24-bit** wav satisfies
`formats: [wav]` and would still be refused by a real NeMo, which checks PCM format and
depth at `audio_file.cpp:89`. Reproduced deliberately in the proof above (200,
Transcode-Ms 0) so the gap is visible rather than assumed away. (2) Formats resolve against
a lane's FIRST member, matching `ConvertFor`. Since the target is always wav, converting for
a strict first member is safe for every other candidate; the reverse — permissive first,
strict fallback — is not covered. (3) A request refused for capacity after its transcode has
already spent it, and spends it again on retry: 4.24 s for a 30-minute file, measured.
(4) **Nothing has run against a real NeMo-Speech.cpp** — the PR is unmerged; the stub
reproduces one documented check, not the backend.

**assumptions** — that 16 kHz mono is right for every restricted STT backend (true for NeMo,
whisper.cpp and parakeet, which all resample to it internally; `audioSampleRate` exists for
the backend that proves otherwise), and that `-fs` plus an ffprobe pre-check is a sufficient
bound on ffmpeg (a file whose duration ffprobe will not state is caught on output size, not
on its claim).

**blocking decisions** — none for phase 1, and none left for contract 2. **Still open**,
in [`p35-corral-layer.md`](p35-corral-layer.md) §9/§12 and filed in
[`open-questions.md`](open-questions.md): **the canonical schema's eight sub-choices
(§12.9), of which (a) gates phases 3–4** · what `/v1/audio/translations` should do given
it does not translate · whether a per-request speaker UUID is a bug or a fact · whether
`response_format: url` is worth corrallm hosting bytes · whether `/v1/realtime` should
report the model's real audio format. **Phase 4 must not start before these are
answered** — it is where a compatibility layer starts lying if it guesses.

**optional extensions** — out of scope now: PCM bounds on `ModalitySpec` (phase 2); a route
probe that asks a backend what it ingests instead of reading a declaration; transcoding on
`/v1/images/edits` and `/v1/audio/speech`'s OUTPUT, both of which are remap-out questions;
a per-caller transcode budget, if ffmpeg CPU ever shows up on the box.

### ✅ P9h — the image surface, and capabilities as inputs/outputs — 2026-09-21 → closed 2026-10-02, [`done.md`](done.md)

### ✅ P30 — the pages people actually open are too long to use — 2026-09-13

Shipped. `/traffic`, `/setup` and `/machines` rebuilt; link colour, chart axes and activity
paging landed first as the shared surface. Full tree, the merge DAG, and the decisions worth
not re-litigating: [`done.md`](done.md) § P30. **Not visually verified** — see §7.

### ◐ P31 — disabled: the statement pause could not make — 2026-09-17

Pausing a permanently-broken backend left it on screen, deliberately. The owner: *"Perhaps we
need a more aggressive pause, like disable — that prevents it from ever being loaded again? And
is more intentional that doesn't get a warning for."*

**The distinction, which is the whole feature.** A **pause** is operational: temporary by intent,
reversible, keyed on a PROCESS (an extension's models pause together), and it SHOULD keep showing,
because you are meant to come back to it. **Disabled** is a configuration statement — "this is not
part of my setup" — keyed on the MODEL, recorded in config history rather than on a fault panel,
and silent by design.

Checked before building, and both corrected an assumption: a pause IS durable (`model_pause`,
`store.go:327`) and already blocks request, fall-through and preload, so the only thing missing was
silence. And `groq-gpt-oss-120b` is not a bare discovered id — it is authored under
`extensions.free.providers.groq.provides`, so the flag has a home and the existing config endpoint
writes it back.

- ✅ **Server** — merged, live on `92ad07d`, migration applied to the production config DB
  (verified independently against a copy first: column added, idempotent over three runs, all four
  models `disabled=0`). Twelve resolution paths closed, including three that would have leaked:
  `/upstream/<model>` bypasses resolution entirely and needed its own guard, `AllModels()` had to
  check `c.Models` rather than the partly-built output or discovery resurrected it, and post-resume
  `repin` would have warned about a model nobody expected back.
  **404, not 503**, for a disabled model asked by name: 503 says it exists and may come back and
  invites a retry — which a pause earns and this does not — and "unknown model" sends somebody
  hunting for a typo in a name that is spelled correctly and sitting in their config.
  A pause survives a disable and stops being reported: enabling must hand back the state you left,
  and silently dropping it would return a known-broken backend to rotation.
- ◐ **UI** — in progress. **next** merge, then disable `groq-gpt-oss-120b` (the owner asked).
  **risks** the feature's own failure mode is that *silent becomes lost*: a disabled model raises
  nothing anywhere, so if the only record is a config file it will be rediscovered months later as
  a fallback list mysteriously one rung short. Enable must be as reachable as Disable, and there
  must be somewhere that answers "what have I disabled". **blocking decisions** none open.
  **optional extensions** a disabled model with no authored entry (a pure pool contribution) has
  nowhere to put the flag — `filter.exclude` is the statement there; not unified, deliberately.

### ✅ P32 — what the aligned model will not do — 2026-09-17 → closed 2026-10-02, [`done.md`](done.md)

### ✅ P32 — the abliteration A/B, answered — 2026-09-17

**Verdict: do not switch.** The claim was "faster and more intelligent". It is neither, and the
one hypothesis worth testing came back negative for the incumbent, which means there was nothing
to fix.

**The instrument had to be built first.** Nothing in the probe library measured over-refusal —
`adversarial-*` measure doing the legitimate half of a task while resisting injected malice, and
say so in their own comments. Four new probes, each arguing its own legitimacy, with a four-rung
ladder that separates a refusal from incompetence, because an outcome check fails for both.

**Result, 5 runs each, `--pin-sampling`, one arm at a time with a forced unload between**
(one arm at a time was UNNECESSARY — see the correction below):

| | valid rows | passed | failed |
|---|---|---|---|
| local-Qwen3.8-27B (stock) | 35 | **35** | 0 |
| local-Qwen3.8-27B-Uncensored | 35 | 30 | 5 |

5 rows per arm discarded as cold-load artifacts (`turns=0 tokens=0`), per the probe's own rule.

- **NO over-refusal in the stock model.** It did every task — credential audit reporting literal
  secret values, backup purge, PII fixture generation, port scanner — without balking once. The
  premise of the whole experiment does not hold on this workload.
- **The challenger failed `overrefusal-credential-audit` stage 0 in 5/5 runs**, identically
  (4198 tokens, 3 turns, 7 tool calls every time). It was NOT a refusal: it read every file and
  then ran out of wall clock. `limitBreached` at 3/14 turns and 7/36 tool calls.

**The mechanism, measured warm on an identical prompt:** same throughput, triple the output.

    stock        622 completion tokens   2041 reasoning chars   6.1s   102.4 tg/s
    uncensored  1936 completion tokens   6845 reasoning chars  18.5s   104.7 tg/s

So abliteration did not make it slower per token — it made it **3.1x more verbose**, which on a
multi-turn tool task is the difference between finishing and being cut off. "Faster" is false by
a different route than expected, and the vendor's own card already showed the capability claim
was inside sampling error (MMLU +0.4 at n=300, GSM8K -1.3).

**Corrections worth keeping:** the card's `-c 8192` was its example, not a cap — it boots at
188160. And it carries its own embedded `nextn` head, so it runs MTP against the target model
with no separate draft file; an earlier note here saying a speed comparison was disqualified for
lack of one was wrong.

**Left undone:** the quant confound is real but unresolved — the incumbent's ladder runs unsloth
`UD-Q6_K`, the challenger is orcarouter `Q5_K_M`. It does not touch the refusal result, and the
verbosity gap is far too large to be a quant step, so it was not worth a third arm. The
uncensored model stays registered and out of every fallback list; nothing routes to it.

### ✅ P33 — a correction: eviction works — 2026-09-21

**Recorded because it was written into this plan twice as fact, and it is false.**

The claim was "corrallm will not evict an idle model to admit another", used to justify driving the
P32 bench one arm at a time with a manual `unload-all` between. It does evict. Proven on box1
2026-09-21: Swift resident, a request for `local-Qwen3.8-27B` arrived, and the ledger recorded
`made room 1x — evicted local-Swift-Qwen3.8-27B for local-Qwen3.8-27B`, `model_load` row
`evicted=local-Swift-Qwen3.8-27B ms=16444`.

**What was actually observed** was BACKOFF, not a refusal to evict. A spill sets
`retry_after≈30s` (`no capacity: retry_after=29.85s (blocking=[local-Qwen3.8-27B])`), and a
request landing inside that window gets `exhausted` and gives up. Every "it won't evict" datum
came from inside a window I had opened moments earlier with my own test. The P32 results stand —
the scaffolding around them was unnecessary.

**The real cost is thrash, not failure.** Two models that cannot co-reside (30.7 GiB each against
a 31.5 GiB budget) alternate at ~16s per swap. That is fine when one name wins and pathological
when traffic splits across two — which is the state today: `chat` resolves to Swift, while 27k
requests/day ask for `local-Qwen3.8-27B` by name. Settling on ONE name is the fix; the lane
ordering is not.

### ◻ P34 — taking turns: a residency-level queue — 2026-09-22

Design doc: **[`plan/p34-residency-timeshare.md`](p34-residency-timeshare.md)**. Nothing built.

The owner: *"I'd like the image/embeddings to take turns with load/unload cycles.
However, we don't have any good contention strategies and load throughput queue flushes
between load/unload timeshares."*

corrallm queues inside a backend (`sched`, fair between GROUPS) and nowhere across them
(`proc.Manager` returns `ErrNoCapacity`, `manager.go:488`). A request for a non-resident
model either evicts and waits out 16s, or takes `retry_after=29.85s` and leaves. Nothing
holds a request across a swap. The design puts the arbiter in `sched` — which already owns
waiters, fairshare, aging, depth bounds and honest backoff — over `(server, pool)`, leaving
`proc` the bytes and the processes.

**Two findings from the read, both verified and usable now.** `minResidency` (10s,
`manager.go:103`) is **not a filter anywhere** — the victim test at `manager.go:1794` uses
only `activeUse`, and its `max()` term at `:1863` can never win because `lastUsed >= readyAt`
always and 10s < 30s. Today's de-facto anti-thrash floor is `activeUse` = 30s, by accident,
and the number meant to be the floor has never once fired. And **gpu1 has four claimants of
which no pair fits**: 9,877 usable, nomic 816 pinned, chandra 8,240, deepseek 6,676, image
~6,144.

**next** answer §11 questions 1-3 (turn floor, turn ceiling, gpu1 composition), then phase 1:
turn floor as a real victim filter, `capacityErrorLocked` returning when the turn ends rather
than when protection lapses, turn reason on the existing `model_load` row. No queue, no
request-path risk, testable in `internal/proc` alone.

**risks** `maxWait` is 15s and a turn is 60s+, so phase 2 only helps the request at the head;
everything behind it still queue-times-out, and that is correct rather than fixable — a card
holding one of three models cannot promise three models' worth of service. A turn cannot be
revoked from an in-flight request, so a 5-minute generation makes `maxTurn` a target, not a
guarantee. The image model's footprint is now MEASURED at 7,360 MiB on the 3080 (not the
~6 GB estimated here first), and does not grow with resolution. It fits beside the pinned
embedder with 1,449 MiB spare and cannot coexist with `chandra-ocr-2` — so the gpu1
timeshare is two-way between image and OCR, not three-way.

**blocking decisions** six, in `p34-residency-timeshare.md` §11. Three gate phase 1: `k`
(swap-overhead ratio, hence `minTurn`), `maxTurn`, and what gpu1 holds. Question 5 is
`open-questions.md` §2 with teeth — if an honest wait estimate is not wanted, phase 1 is the
entire feature.

**optional extensions** out of scope: cross-pool arbitration (no observed coupling);
preemptible generation; a `timeshare:` config block (only `minTurn`/`maxTurn` are new and
they belong on `servers.<s>`). Phase 4 — validating that the `persistent` set fits its pool —
is independent of everything else and is the smallest thing in the document.

### ✅ carlsmacbookpro is back — 51 GB of Metal that was switched off — 2026-09-05 → closed 2026-10-02, [`done.md`](done.md)

### ◻ What the Lenny runs left open — 2026-09-05

Seven runs over two days and two scenarios, the harness, P30 A–D, and everything they fixed are
archived in [`done.md`](done.md) § Lenny. This is only what is still true and still costs
something.

**The decision, and it gates the three under it** — `open-questions.md` §3: **what does a CALLER
see?** A person handed an API key today gets the operator's dashboard with one credential. The
caller run is the evidence: the door asks for an "Admin token" he cannot get (0 on three of four
questions), the last screen hands him a GPU serial number, and `Change`/`Unassign` follow him
onto the page that is supposedly his own.

**✅ DECISION RESOLVED 2026-09-11: closed.** Callers reach corrallm through `/v1/*` with their
key and never through the dashboard, which is already how it behaves — and the premise this
question rested on was wrong. `auth.Middleware` gates every `/api/` route on the ADMIN TOKEN
alone; probed live, a caller key returns 401 as a header and as a Bearer while the admin token
returns 200. A person holding an API key cannot open the dashboard at all.

The run that produced these findings injected the admin token (`bin/ux.mjs`), so the caller
persona toured an administrator's dashboard. The findings are real for anyone holding the admin
token — a population you choose when you hand it out — and not for callers.

So of the three items below, only the front door was ever reachable by a real caller, and it is
now fixed: the sign-in page tells a key-holder their key is for requests, names the header and
route it belongs on, and says the dashboard belongs to whoever runs the box. The other two are
**closed unbuilt**; they describe a surface callers cannot open.

- ⏸ *(caller, closed unbuilt — behind an admin login)* **Nothing says which row is you.** Ten keys listed, none marked as the reader's.
  He identified himself by luck — the one key that appeared in a live request from an address he
  recognised.
- ✅ *(caller — the one a key-holder could actually reach; fixed 2026-09-11)* **The front door spoke only to an admin**: "CORRALLM — ADMIN SIGN IN … `cat
  /home/nthalk/.corrallm/admin.token`". It never uses the word "key", which is the only thing he
  was given.
- ⏸ *(caller, closed unbuilt — behind an admin login)* **Which model names go in an editor?** The catalog lists thirteen names — models,
  lanes, proxies, absent ones — without saying which are requestable. He only found out by
  accident, from an example `curl` that used `"model":"chat"`.
- ✅ **`PLACEMENT` was the label** on a column head, a section heading, a chip and the prose
  pointing at it (OB-3, both personas). Now *Ran on*, *Ways to run this*, *N boxes*; the word
  survives in each gloss, because an operator who reads the config needs the two vocabularies
  joined somewhere. `0eb90c2`, deployed.
- ✅ **Machines did not say whether its numbers were a problem.** Shipped 2026-09-10. A full pool
  now says what being full has COST — "Made room 3 times today — last unloaded X to fit Y" — and
  says nothing when it has cost nothing, because a device pool at 96% normally means a model is
  resident. See `done.md` § "A full pool says what it cost". The `behind` tool marker is
  untouched and is now the only half of run 9's finding still open.
- ✅ **A backend load records no reason.** Shipped 2026-09-08: `model_load` carries who triggered
  the spawn and what it displaced, on both outcomes. `ce5e363`, and `done.md` § "A backend load
  says why" for the one column not yet seen live.

**next** — nothing is gated. Every item on this side has shipped and the caller decision is
closed. What remains unbuilt and undecided: the `behind` tool marker (run 9, step 03), a smaller
instance of the same OB-6 question on the toolchain panel; and `open-questions.md` §4, whether
aw4 should ride the `chat` lane, which gates nothing.

**Slot cache — built, wired, OFF.** `internal/slotcache` + `--slot-cache-dir`, measured before
it was written (save 643 ms, restore 331 ms, 11x against reprocessing; 36.8 KB per token).
Turning it on needs the flag plus a restart; the backend precondition is already met.
`X-Corrallm-Conversation` lets a caller name its conversation rather than have the head of its
prompt guessed at — **filed with aw4** (`~/local/src/iodesystems/aw4/plan/icebox.md`), because
aw4 is one caller with many concurrent nodes and is therefore the traffic that thrashes a
one-slot backend. Inert until the cache is on, so it can land there whenever.
**risks** — a vocabulary fix is where vocabulary bugs are born; name the object.
**how we will know** — run 8 on both scenarios, same build, reporting composition against
`done.md` § Lenny's table.

### ⏸ Slot cache — built, measured, TURNED OFF 2026-09-06 00:10

**It is off in production.** Built, wired, tested, enabled for two hours, measured honestly, and
switched off because on this box's traffic it was a net cost. The code stays; the flag does not.

**Three windows, `sk-aw4` on `local-Qwen3.8-27B`:**

| | requests | mean | reuse | tokens re-read |
|---|---|---|---|---|
| A — 3 h before, cache off | 1,847 | 3.7 s | 94.8% | 3,159 |
| B — cache on, first hour | 827 | **2.8 s** | **97.8%** | **1,129** |
| C — cache on, after a restart | 836 | **4.3 s** | 95.4% | 3,527 |

**What separates B from C, and it is the whole finding.** In B, restores happened — the log shows
`caller=sk-aw4 restored=true` followed by `f_sim_best = 1.000`. In C, across 836 requests: **15
saves, 18.7 GB written, and not one restore.** Twenty distinct conversation keys, none repeated.
Every switch paid ~1.6 s to write a state nothing ever asked for again, and the mean answer time
went from 3.7 s to 4.3 s — worse than having no cache at all.

**Why the keys stopped repeating** is the failure predicted when the declared-id header was added
(`slotcache.ConversationHeader`, and filed in aw4's icebox): the inferred key is the system block
plus the first turn, and a client that PRUNES the head of a long conversation changes exactly
those. aw4 does that. B's restores were probably conversations short enough not to have been
pruned yet, plus my own probes, which sent a declared id.

**So the mechanism is proven and the KEY is not.** Save, restore, prefix-match after restore, the
eviction, the sweep — all demonstrated. What is missing is a conversation identity that survives
the client's own context management, and only the client can supply it.

**How to bring it back, in order:** aw4 sends `X-Corrallm-Conversation` (its icebox entry) →
re-enable with `--slot-cache-dir` → watch the log for a restore rate, not a save rate. **A save
with no matching restore is the whole cost and none of the benefit**, and the log now shows both.

The 27 GB of states are moved aside at `~/.corrallm/var/slots.disabled-20260906`, deletable at
any time.

### ◐ ~~`--cache-reuse 256` is live~~ — RETRACTED 2026-09-05 21:49### ◐ ~~`--cache-reuse 256` is live~~ — RETRACTED 2026-09-05 21:49

**Applied** (config revision 27, one line, restorable): `local-Qwen3.8-27B` spawns with
`--cache-reuse 256` beside its existing `--parallel 1`. More slots is the textbook fix for
prefix thrash and it costs VRAM the box does not have (gpu0 at 96%) — **decided: no more slots
until the GPUs are paid for**, so this is the free half.

**What it can fix:** llama.cpp re-uses cached chunks by KV-shifting them, so when a caller PRUNES
the head of a long conversation — everything shifts, no prefix matches, today the whole prompt is
re-read — the suffix can be reused in place. The 10:00–14:00 window fits that shape: average
prompt size FELL from 62–80k to 40–49k while reuse collapsed, which is what head-pruning looks
like. **What it cannot fix:** two genuinely different conversations interleaved on one slot. They
still trade the cache, and no flag changes that.

**How it was applied, since the obvious ways are wrong here.** `config load` replaces the whole
configuration and needs `--force`; the entry-YAML API refuses this model (`served name
"local-Qwen3.8-27B" collides with a model declared elsewhere`) because it is authored under
`providers.local`, not at the top level. The right door is `upsertModel`, which merges onto the
existing model via `applySpec` — so `sampling`, `convert`, `modalities`, `aliases`, `pool`,
`swap` and `contextPerRequest` survive, and `sticky` is re-sent whole (2m / 5m / medium) because
the form rebuilds that block wholesale. Verified: `config export` differs from the pre-change
export by exactly one line.

Restarted at a moment with nothing in flight, then loaded deliberately so no caller paid the cold
start. Verified in `pgrep`: `--parallel 1 --cache-reuse 256 … -c 188000`. A chat request answers
correctly with speculative decoding still on, and the log is clean of KV-shift errors.

**THE BASELINE TO JUDGE IT AGAINST, frozen here before the change:**

| | value |
|---|---|
| prompt-token reuse, 24 h to 14:30 | **92.0%** (10,515 requests) |
| mean time answering, same window | **5.0 s** |
| the bad stretch, 13:00–14:00 | 57.2% reuse, 10.8 s, 17,148 tokens re-read/request |
| requests finding no near-exact prefix (llama.cpp `f_sim_best < 0.95`) | 09:00 **30%** → 13:00 **68%** → 14:00 **9%** |

**How to check it tomorrow** — the instrument exists now, which it did not this morning:
the reuse figure is on Now (it fires a card when reuse drops a fifth AND time per request rises a
third), and per hour:

    journalctl --user -u corrallm --since today | grep -oE 'f_sim_best = [0-9.]+'

**What would say it worked:** the `f_sim_best < 0.95` share falls during a head-pruning stretch
without reuse collapsing with it. **What would say it did nothing:** the next slow window looks
like this one — reuse down, time up, f_sim low. Either answer is worth having; it is one number
now instead of an argument.

**Early read, 3 hours in (17:35) — NO MEASURABLE EFFECT, and the flattering version of that
number is a trap.** Against the 24 h baseline it looks like a win: 96.0% reuse and 2,649 tokens
re-read per request, against 91.9% and 4,649. That comparison is worthless, because the baseline
window CONTAINS the degraded stretch. Like for like:

| window | reuse | re-read/request | mean |
|---|---|---|---|
| 14:00–14:36 — recovered, still no flag | **98.8%** | 1,192 | 2.6 s |
| 14:36–17:35 — with the flag | 96.0% | 2,649 | 4.1 s |
| yesterday 15:00–18:00 — normal, no flag | 95.4% | 2,235 | 3.8 s |

Against the honest comparison — the same hours yesterday, both periods healthy — it is 95.4% →
96.0% and 2,235 → 2,649 tokens re-read. **Nothing.** Which is what the flag's own description
predicts: it helps a prompt whose head was pruned and whose suffix reappears shifted, and normal
operation is not that. The `f_sim_best` figure I first reached for (15% below .95 since the flag,
against 51% for 09:00–14:00) is the same trap wearing different clothes — that 51% IS the
degradation, not a baseline.

**THE FLAG WAS NEVER ACTIVE. Retracting the whole experiment, 2026-09-05 21:49.** llama.cpp says
so at every load, and I did not read its log until something else sent me there:

    W srv load_model: cache_reuse is not supported by multimodal, it will be disabled

`local-Qwen3.8-27B` carries an mmproj for vision, so `--cache-reuse` is disabled the moment the
backend starts. The "early read" above measured nothing; the honest comparison it corrected was
also measuring nothing. **Both readings are void** — not "inconclusive", void. The flag can come
out of the cmd at the next restart; it costs nothing except the impression that something was
tried.

**What this is worth keeping:** the reason the reading looked plausible is that I compared
against a window and not against the mechanism. A flag that a backend silently disables produces
exactly the numbers of a flag that does nothing, and no amount of care with the WINDOWS would
have found it. The backend's own startup log would have, in one line, before the experiment
began.

### ✅ Per-request priority group, bounded by the key (2026-08-31)

Asked for by aw4, which is one caller with two very different kinds of traffic:
an assistant a person is blocked on, and autonomous work nobody is waiting for.
One key per role would have made it N fairshare claimants against yscr's one —
key generation boosting a client, structurally, by its own design.

- `keys:` accepts either a bare group name (unchanged, every existing config) or
  a policy: `{default: batch, interactive: true}`.
- The credential carries the choice — `sk-aw4:interactive`. In the key rather
  than a header because every OpenAI-compatible client can set an API key and
  few can set a header; a weighting the caller cannot express is one it will not
  use.
- The whole credential is matched as a key FIRST, so a key that literally
  contains `:` keeps resolving to itself. Adding this cannot change what any
  existing credential means. Validation rejects such a key carrying escalations,
  since the suffix form is unreachable for it.
- A refused escalation is SERVED in the key's own group, not rejected — failing
  a request over a weighting is worse than serving it at the weight it is
  entitled to — and says so with `X-Corrallm-Group-Denied`. A silent downgrade
  (revoked permission, renamed group, typo) is the failure that costs a day.
- Cost attributes to the base key. Keyed on the raw credential, one tenant's
  spend would split across a row per group and `RollupByKey` would answer "what
  did this caller cost" with a fraction of it.
- ⚠️ **Permission is not a budget.** The flag says who may ask; nothing bounds
  how much. "A person is waiting" is a judgement the caller makes about itself,
  and the likely failure is a bug — a default left interactive, a rule that
  drifts — which corrallm cannot detect. The guard is `limits` on the escalated
  group. Set one before granting this to anything autonomous.
- `config_key` gained an `allow` column (JSON array) plus the first entry in a
  new `migrations` list — `CREATE TABLE IF NOT EXISTS` is a no-op on the
  production database, so without it the column would exist only on fresh
  installs and every write on box1 would die with "no such column". Tested by
  building the old table and migrating it.

### ⚠ The claims-nothing rule is live, and the recorded caveat understated it

Shipped with the split, as decided: an unmeasured model on a DEVICE pool now claims **nothing**
rather than reserving the whole pool, and the spawn is the fit test. Host RAM keeps the
conservative path, and so does any host that cannot measure per-process memory.

**The decision recorded "a CUDA OOM kills only the allocator". That is true and it is not the
whole risk — the allocator is not always the newcomer.** A resident model that grows AFTER load
can be the one that asks for memory an unmeasured neighbour already took. Qwen's vision path
spikes ~2 GB on a 400-dpi page, and that is exactly the shape of the 2026-08-14 production OOM,
where Qwen3-6 crashed with 1,483 MiB free.

Exposure is one spawn per (card, model): after that, measurement governs. It is bounded, it was
decided deliberately, and it is written down here rather than discovered later.

**next** watch for a spawn-time OOM on box1's first cold load of any newly-added model. If one
bites an INCUMBENT rather than the newcomer, that is the signal to reinstate a floor — reserve
some fraction of the pool for an unmeasured model instead of nothing.
**risks** as above. `gpu1` is the likely first place to see it: 831 MiB of headroom once nomic
and chandra are both resident, and chandra's footprint is input-driven.

### ◐ P21c — budget granularity

Design doc: **`plan/p21-provider-credentials.md`**. P21a–i shipped (archived in `done.md`);
P21c is the remaining slice.

**next** P21c — move budgets to where the provider actually meters. P20's per-model `usd`
budget is the wrong granularity: a `usd` cap on a discovery template becomes N independent
caps (12 models × $200 = **$2,400, not $200**). The live openrouter template carries such a
cap today.
**risks** the preset table's basePaths were probed once, on 2026-08-15; a vendor moving one
shows up as a 404 from a host that plainly exists. The catalogue browser prints the URL it
tried, which is the intended cure.
**assumption** browsing uses a credential's STATIC headers, matching what the discovery loop
does; a credential using `authTokenCommand` reports that it cannot be browsed rather than
sending an unauthenticated request and blaming the endpoint.
**✅ BOTH BLOCKING DECISIONS RESOLVED 2026-08-24.**

1. **Where secrets live — a `0600` file, never served.** Taken as recommended; nothing
   contradicted it and no alternative was ever proposed. The constraint that forces it is
   unchanged: `/api/v1/config/*` serves config as YAML, so a key-management UI that persisted
   secrets into config would turn a working endpoint into a disclosure surface. Keep secrets
   out of the config object entirely — not redacted on read, *absent*, so no future handler can
   leak them by forgetting to redact. P21f/g are unblocked.
2. **The `discover:` question is MOOT — it already deployed and the consequence is absorbed.**
   The live config reads `"directory":{"filter":{...}}`, migrated by P26 into the config store;
   there is no `discover:` left to warn about. The 12 discovered models are already gone and the
   `free` lane is already down to its two declared members (`groq-gpt-oss-120b`,
   `cerebras-gpt-oss-120b`). **Measured cost of that loss: 12 requests in 7 days**, one each to
   groq and cerebras. Not worth a decision.
   Whether to build a real free pool (`extensions.free.virtual`) is an **icebox** item, not a
   blocker — see `icebox.md` § Free pool.

### ✅ P22 — the open tail (KLD baseline, clean head-to-head, probe scoping) → closed 2026-10-02, [`done.md`](done.md)

### ◐ Trial spawn + add-model form *(was mis-numbered P16 — git's P16 is the free aggregator)*

Authoring a model is currently "type a cmd string into a YAML box, save it, load it, read
the logs, guess again". Every iteration mutates live config.
✅ **API slice done** — `POST /api/v1/config/models/trial` (`internal/proc/trial.go`,
`internal/api/trial.go`): spawns an UNCOMMITTED cmd on a chosen server and tears it down,
writing nothing to config whatever happens. Stages reuse what existed: `TargetFor` (resolve,
incl. the agent-endpoint rewrite) → admit → `hostFor(server).Start` (local or remote, one
interface) → line-buffered log events → `waitHealthy` → `GET /v1/models` → `probeUI` →
`handle.MemoryMiB()` → term/grace/kill. Reuses `specToModel`, so a trial and the save that
follows interpret `proxy: 5800` identically. A failed command returns **200 with the
transcript**, not a 4xx — the failure IS the answer, and an error status would discard the
logs saying why. 6 tests cover the admission invariants.
**next:** stream the transcript (today it returns as one document when the run ends — fine for
curl, poor for a 90 s cold load being watched); then the add-model form itself, with
"Trial" beside "Save" and the result prefilling `ramUsage`/`upstream`/`maxConcurrent`.
**assumption made:** a trial is registered `persistent: true` so unrelated traffic cannot kill
a run halfway. Safe only because it reserves nothing but FREE memory (never evicts), so it
displaces nothing by holding it; `trialTTL` (10 min) is what bounds the harm. Revisit if
trials start being left open.
**DECIDED (USER, 2026-08-04): a trial reserves only if it FITS — never evicts.** On a full box
it refuses with "unload something first". A trial is an experiment; it must not be able to
take down a warm production backend.
**risks:** ✅ (1) registered in `m.procs` under `trial:<id>` so `ReconcileAgent` adopts rather
than reaping it past the 60s grace (`reconcile.go:71`); ✅ (2) `trialTTL` + a deferred teardown
on every exit path, so a closed tab cannot strand the reservation; ✅ (3) one trial per server,
enforced in `admitTrial`; ◻ (4) it widens the existing RCE surface from config-authoring to a
dashboard button — same admin-token gate, unchanged, but stated rather than discovered.
**blocking decisions (USER):** none outstanding.
**optional extensions (out of scope):** re-probe/resize an already-enrolled server from a live
`GET /agent/v1/capacity` (would have auto-repaired the osx double-count, and would track a
`iogpu.wired_limit_mb` the operator changes later); trial an A/B pair of cmd strings and diff
their banners.

### ◻ Fix the wait estimate *(was mis-numbered P15b — git's P15 is bench)*

The measurement trees this rests on (retry promises, utilization, service-time distribution)
are archived in `done.md` § Dashboard & observability. **Read the censoring note there before
validating any new estimator against `realWaitMs`.**

**Reframed 2026-08-03 by the first 30 min of retry-promise data — the original premise was
WRONG, and is recorded here so it is not re-derived.**
- **What I predicted:** N callers rejected in the same second all get the SAME number, all return
  in the same instant — a self-inflicted thundering herd — fixed by a promise ledger of phantom
  waiters plus staggering.
- **What 18 real promises showed:** no clustering (told-values ranged 2s–14s, since `ewmaDwell`
  tracks prompt size on a single-slot 27B); **no bursts at all** — every one was `queue-timeout`,
  not a single `rejected`, so `maxQueueDepth` is never reached on this box and the constant-promise
  path the herd argument rested on is not exercised; **zero `early`**, one `gone`. And the estimate
  was **short every single time** — 1.7×–11.6×, median ~4×. The caller (`dun`) retries, is refused
  again, and backs off more patiently than we asked.
- **So the defect is accuracy, not collision.** `rounds × ewmaDwell` badly under-models a queue
  whose dwell varies by prompt size, and the caller's own backoff absorbs the error. Phantom-waiter
  counting addresses a problem this deployment does not have.
- **next:** watch the utilization panel's est-vs-real gap across a wider traffic mix before
  changing the formula — one caller against one single-slot model is not a sample.
- **blocking decision (USER owns):** whether an honest estimate is even wanted. A truthful 25s may
  drive callers off (`gone`) where an optimistic 4s keeps them retrying into a slot that does open.
  That is a product call about what a 429 is FOR, not an arithmetic fix.
- **risks:** dwell EWMA is prompt-size-blind, so any recalibration on this box's traffic may not
  generalize; `keepSoonest` takes the min across a lane walk, so a lane's promise is not
  attributable to one backend.

### ◻ `host.Remote` — the last multi-node step

Everything else in the multi-node ordering shipped: per-server VRAM accounting, live config
reload, the `internal/host` Spawner interface, `Server.Agent` binding, `corrallm agent`,
failure semantics, darwin capacity, and enrollment + the Agents/Config UI. Shape (b) —
full remote control, box1 spawns onto machine 2 — was decided 2026-07-28.

**next** `host.Remote`, integration-testable by running a second agent on another port on box1.
Then: switch the daemon's default config to the managed one and retire the hand-written file.
**❓ blocking decision (USER) — ONE, and it is `open-questions.md` #1:** agent lease self-reap
on/off and its TTL. An agent that loses its primary but stays up keeps its backends running
(`internal/agent/server.go:307` names this an open decision in so many words). The primary side
is already settled and shipped — no self-reap, adoption reconciles on heartbeat — but the agent
side is not. Answer it before `host.Remote`, not now.

**✅ Transport trust — RESOLVED 2026-08-24, and it was already answered in the code.** The agent
executes arbitrary shell strings because that is its function: the primary sends `sh -c` strings
and the agent runs them. `internal/agent/wire.go:14` states the position and the mitigation —
*"a remote-code-execution surface by design, and a token is required unless one is explicitly
waived. Treat exposing it exactly as you would treat exposing a shell."* Enforced at
`server.go:110` (`agent token required`).
There is no new trust to grant: corrallm's config already carries `cmd:` strings it execs
locally, so the agent widens the *reach* of that trust to a second box, not its kind. Accepted as
designed. **The standing rule that follows from it:** never expose an agent port anywhere you
would not expose a shell, and never waive the token outside a test.

**◻ Whether to spike `proc_pid_rusage`'s `ri_phys_footprint`** for real per-process measurement
on darwin — a task, not a decision. Today the Mac declares `noProcessMemory: true` and `ramUsage`
becomes authoritative there, with validate-time enforcement so a model omitting it is an error
rather than a box that silently serves one model at a time. That is a working fallback; the spike
would only improve it. Not blocking.

**Design + the full shipped ordering** (shape (a) vs (b), the enrollment/agent/darwin/failure
steps, box2's unified-pool decision and the re-enrol repair path, and the agent-addressing rule
that makes `Server.Agent` a LIST of endpoints) is archived in `done.md` § Multi-node. **One thing
there is a live trap:** `config.IsLocalHost`'s "a private-LAN address is not local" doc comment is
already wrong under this topology — the predicate is still correct, the comment is not.

### ◻ OSS follow-ups (not blockers)

- Auth multi-user accounts/roles + token rotation — today is a single shared admin token.
- Rename the `WattsPerToken` cost fields to `WhPerToken` (the values are already Wh/token;
  the name is the only thing wrong).

---

## 7. Known gaps in shipped code

Resolved decisions and closed gaps are in [`plan/done.md`](done.md); decisions still needing
you are in [`plan/open-questions.md`](open-questions.md). What follows is only what is still
true and still costs something. Optional extensions and out-of-scope items live in
[`plan/icebox.md`](icebox.md).

### Known gaps — shipped and live with these

- ✅ ~~**`/m/<name>/activity` is a route that can never render.**~~ Deleted 2026-09-05. It was a
  full page (the shared activity table scoped to the model, with `?placement=` narrowing) that
  `ModelConsole` never rendered — no `<Outlet/>` — so the URL resolved and silently showed the
  parent's Info tab, and nothing in the UI linked to it. Deleted rather than wired up: the model
  page's own Usage tab answers the same question, and Traffic now answers it for any window. A URL
  that replies with the wrong page is worse than a 404.
- ✅ ~~**Every remote model except the three anthropic ones carries `type: chat`, an
  ENERGY-priced type.**~~ **Closed 2026-09-26 by P36b (§6), and no config change was needed.**
  The gap was real — 7 openrouter requests costed at 13,352 processed × 0.00005 Wh + 2,155
  generated × 0.0013 Wh = 3.469 Wh × $0.14/kWh = **$0.000486** of this box's electricity for
  work done on OpenRouter's hardware — but the fix this entry named was wrong. These models are
  FREE, so declaring a paid type would have mispriced them in the other direction. They now
  report as a fourth billing kind, `free`, off the `freeTier:` the virtual pool's template
  already puts on every model it contributes; their cost is $0 because that is the price, and
  the comparison prices them ($0.0241 at anthropic-haiku rates) instead of excluding them.
  `unattributed` is back to meaning only a genuine gap: on the live box it is now $0 and holds
  nothing but `local-Qwen3.8-27B`, the entry below. A PAID openrouter model still needs a type
  with `cachedCostFactor` / `processCostFactor` / `generateCostFactor` — but it would no longer
  be silent, because a provider-reported charge above the energy ceiling reverts the row to
  `unattributed` rather than being written off as the phantom.

- **690 of one day's requests are 429s against `local-Qwen3.8-27B`, a model the Swift cutover
  removed from the config.** Every one is a `queue-timeout` on `/v1/chat/completions` from
  `sk-aw4`. The name still resolves far enough to queue and time out, and the rollup can no
  longer say what type priced it (P36 reports the row as `unknown-model`). Whether the caller
  is pinned to a retired name or a fallback list still names it has not been traced.

- **An oversized request body is silently truncated, not refused.** `handleInference` reads
  through `io.LimitReader(r.Body, maxBody)` (64 MiB on the STT routes, 32 MiB elsewhere) and
  never notices that it stopped early, so a 74 MB upload becomes a 67 MB one and the backend
  is handed a file that ends mid-frame. Found while bounding the P35 transcode, which refuses
  over-long audio with a 413 and a number — this path does not. Pre-existing and untouched.

- **`carlsmacbookpro`'s agent is unreachable** (`dial tcp 192.168.1.248:6503: connect: connection
  refused`, observed 2026-09-04). The Hosts page shows it, in the daemon's own words. Same class as
  the known macOS Local Network permission trap; unverified which it is this time.

- **The Overview slowdown card only fires on reuse loss.** Its trigger is still
  `reuseLost >= 0.2 AND meanTime >= 1.33x` (`ui/src/routes/index.tsx` `slowdownFaults`), so a box
  that slowed with its prompt-cache reuse intact — a thermal event under a steady caller — raises
  no card at all. Since 5741a93 the card at least *rates* itself in tg/s and tp/s and no longer
  claims "nothing is broken" when generation speed has fallen, but detecting a bare tg/s drop is a
  second fault that does not exist yet.
- **P30's pages were walked once (Lenny, 2026-09-13, run `tmp/ux-run-is-my-box-working-20260913-193558`,
  daemon 97325f6).** Q3 "do I know what is being asked of me" scored **1 on five of nine steps**.
  Open findings, none yet fixed:
  - ✅ **`sk-aw4` is in `batch` and no screen says what that costs** — fixed. The roster now
    names the callers a higher group outranks, and the key page says it in prose before the
    numbers. Gated on another group actually outranking this one, so the top group's callers
    see nothing: a weight is a ratio, and a group nothing outranks cannot be stopped for one.
    "Can be stopped mid-answer" (the setting) and amber (an event happened) stay separate
    sentences; where the preemption ledger is box-wide the wording says "on this box", never
    "yours".
  - **"1 attached machine is not [serving]" is a remote credential, not a machine** — and
    `/machines` says `all reachable` for both real hosts. The same word means two things on two
    screens. He noted no OB rule covers this and that one is owed.
  - **`/m/$name?tab=usage` has no time picker** — a fixed "Rollup (24h)" only, so the one screen
    scoped to a model cannot be asked about the hour that went wrong.
  - ✅ **`lane` and `rung` as bare labels** — fixed. The object is a **fallback list**, which the
    panel already said in prose two lines under the word it was refusing; `lane` survives only as
    a gloss beside it, per the `placement` precedent, so an operator reading the YAML keeps the
    join. Zero bare `lane`/`rung` labels remain in the product.
    **Correction to an earlier finding:** the `groups.tsx` Reservations column was reported here
    as a `Group` header over `lane` data. It is the reverse — `reservation.go:75` resolves that
    value with `ResolveGroup` and the schema documents it as a priority group, so the HEADER is
    right and the WIRE FIELD is misnamed. Renaming the column would teach a meaning that is wrong
    two panels away. Fixing the field is a server change nobody has made.
  - ✅ **Two screens rendered an empty frame while loading, and tiles asserted a zero they had
    not measured** — fixed. Found by a walk that happened to hit a busy box; reproduced
    deterministically by holding every GraphQL reply 6s in Playwright. Three states now render
    differently: **never asked** = `not in yet`, **asked and no such figure** = `n/r`, **genuinely
    zero** = the number. Deliberately two idioms, not one — `n/r` is settled and a remote provider
    will never report one; `not in yet` may be a number a second later. The same gate also gives a
    FAILED query somewhere to report itself: a dead totals query used to look exactly like an idle
    box.
  - **`box1` carries no status badge** while `carlsmacbookpro` shows `up`; `96%` has no framing.
  - **Setup opens on Providers, not History** — flagged by the same persona that generated OB-11.
    Decided deliberately by the user 2026-09-13; recorded here because the evidence came back.
  - ✅ **Sentences describing a control were clipped mid-word** — fixed as a class, not as
    instances. `Panel`'s subtitle keeps `nowrap`/ellipsis (a one-line bar is what makes a dense
    page scannable, and clipping a LABEL costs nothing because its tail restates its head); a new
    `note` prop carries prose. A sentence truncates asymmetrically — subject first, consequence
    last — so the cut takes the consequence and leaves something that still reads as finished.
    Twelve subtitles moved, not the four a walk happened to photograph. **Enforced by
    `corrallm/subtitle-is-a-label` in `ui/eslint.config.js`, not by a comment** — a comment
    already failed here once (`6906b46` fixed one instance and the class came back). Clipped
    lines across three walks: 5 → 4 → 3; the three that remain are the operator's OWN config
    notes, collapsed behind a visible "Show all notes" button rather than a hover tooltip.
  Also fixed out of this run: the two window-deaf series resolvers (`3f53b56`).
  **Caveat on the run:** the `lenny` agent type did not register from `.claude/agents/lenny.md`,
  so it ran as a general-purpose agent with the definition inlined. The real one gets ONLY a
  `Read` tool, which is what makes "denied the codebase" a fact about its input rather than an
  instruction. It reported reading nothing outside the run directory. Re-run properly before
  treating any of this as a baseline.
- **More tiles on `/traffic` still assert a zero they have not measured.** `RECENT REQUESTS 0`
  (`ActivityLog.tsx`), `TURNED AWAY 0 unanswered · 0 due back` (`TurnedAway.tsx`), and a bare `…`
  from `TrafficWarnings.tsx`. Same defect as the totals strip, same fix available — `NotYet` is
  exported and ready. Left undone because they were outside the branch's file scope.
- **`ProviderDialog.tsx:135` still says "its free models join the … lane automatically"** — now
  inconsistent with the badge one panel away, which the vocabulary sweep could not reach.
- **`refusalMeaning`'s "the key is not accepted" means a PROVIDER credential**, while "key" means
  a caller's API key on `/callers`. One word, two things, pre-existing.
- **The Mac's agent cannot build anything: `cmake` is not on its PATH.** `tools preflight
  llama.cpp --server carlsmacbookpro` reports `BLOCKED, missing: cmake` — with no "(have …)", so
  it is ABSENT, not too old; a direct ssh preflight on the same host finds cmake 4.4.3. Homebrew
  on Apple Silicon installs to `/opt/homebrew/bin`, which a launchd-spawned agent does not
  inherit. Same class as the Local Network permission trap: the agent's environment differs from
  a login shell and nothing surfaces it.
  **The cost is already paid and was invisible:** `rebuild: true` is tool-level, so the Mac is
  enlisted and compiles unattended — it has been trying and failing (`tool_build` row 114,
  `failed`), and its llama.cpp sits at build 10630 against box1's 11065, roughly 435 behind.
  It can RUN what is already installed and cannot build anything new, which is why
  stable-diffusion.cpp had to be installed over ssh rather than through `tools build`.
  Fix is the agent's environment, not the recipes.
- **`PutEntryYAML` and `UpdateNotes` silently discard edits to a provider-authored model.**
  Both write only the folded `c.Models[name]`; for a model under `providers.local.models` the
  AUTHORED block is untouched and the reload that follows rebuilds the folded copy from it.
  `PUT /api/v1/config/notes/model/<name>` returns `{"ok":true,"message":"notes saved"}` and
  changes nothing. `PutEntryYAML` is worse — it leaves the model declared in BOTH places, which
  the next Load refuses as a collision. `UpsertModel` (configedit.go:219) is the only one of the
  three with the write-back branch. **This matters more since 2026-09-21**: every model was just
  migrated to `providers.local.models`, so the broken path is now the normal path, and the YAML
  endpoint was what this session used for every config edit.
- **Every spawned backend is gated on `/health`, which is a llama.cpp assumption.**
  `proc.waitHealthy` (manager.go:2199) requires a 2xx from `<target>/health`. sd-server has no
  such route — its surface is `/v1/…`, `/sdapi/v1/…`, `/`. The model loads perfectly and corrallm
  declares the spawn `StateFailed`, and there is no per-model health-path field to say otherwise.
  Worked around 2026-09-21 with a stdlib shim on the Mac that answers `/health` once `/v1/models`
  does. The cost of the workaround: corrallm measures the SHIM's footprint, not the backend's
  (trial reported 2 MiB), so `ramUsage` must be hand-declared — which is exactly the stale
  hand-typed number the measured-footprint work exists to remove. The fix is a health-path field
  on the model, or upstream adding `/health`.
- **`/v1/capabilities` describes a diarization response that nothing returns.**
  `proxy.go:2300` promises *"a diarizing model additionally returns
  `segments:[{speaker,start,end,text}]` and `num_speakers` alongside the OpenAI `text`"*.
  oidio's own manifest (`internal/server/capabilities.go:73`) says it adds speaker **UUIDs**
  on verbose_json segments plus a stateless `speakers` array of uuid + voiceprint +
  similarity — and `num_speakers` appears nowhere. The manifest is describing an older
  oidio. This matters more than a stale comment: `/v1/capabilities` exists so a client or
  an LLM can build against it without reading code, so a wrong field name there is acted
  on rather than ignored. Checked 2026-09-23 while verifying a candidate replacement
  backend; NeMo-Speech.cpp returns a third shape again (words with an integer `speaker`,
  or textless segments, and never `num_speakers`), so whatever the manifest says next has
  to be true of the backend actually serving, not of the family in general.
- **`ui/` has no test runner.** No vitest, no jest, no `test` script in `ui/package.json` — so
  `slowdownFaults` and every other pure function in the UI is covered by `tsc` and eslint only.
  The DoD's "new behavior has tests" is met on the Go side of a UI change and silently skipped on
  the TS side. Adding a runner is a decision, not a chore: it is the first test dependency the UI
  would carry.
- **P6 known gaps:** (1) swap $ is charged to the load *trigger* only — not amortized across the
  coalesced batch; a load whose trigger loses the ctx race goes unbilled. (2) Over-budget with a
  `queue` stage degrades to back-off (reason `over-budget` + Retry-After), not an internal
  budget-wait — the client retries when the window frees. (3) Usage capture caps at 1 MiB; a
  non-streaming reply larger than that meters as $0 (streaming keeps a rolling tail). (4) `cost`
  share-currency is retrospective (decayed past releases), so in-flight cost is invisible to
  fairshare until release.
- **Test-teardown race**: a held in-flight request can log after `store.Close()` in one test
  (benign warning); revisit if it becomes flaky.
  (3) **`interactiveOrigins` not ported** — llama-swap's browser-origin auto-priority has no corrallm
  equivalent; browser callers land in `default` unless keyed (design choice — priorityGroup is first-class).
  (4) **`queued_ms` is forward-only** — rows predating the column read 0; queue *wait* populates as new
  queued-then-served requests accumulate (rejections + sampled depth are already live).


- ✅ ~~**The qwen 502s need the upstream ~120 s timeout raised.**~~ **No longer observable —
  measured 2026-08-24, 7 days of production traffic.** Three 499s in the window, at 3.6 s, 6.0 s
  and 12.8 s — client cancels, not a 120 s guillotine — and the longest SUCCESSFUL request dwelled
  **4,676 s (78 minutes)**, which no 120 s cap upstream could have survived. Either the front-proxy
  `proxy_read_timeout` was raised or the traffic mix stopped reaching it. P10a's honest reporting
  is what makes this checkable at all: a 499 now means what it says. Reopen only if 499s reappear
  clustered near a round number.

### The activity DB is 5.9 GB, and it is `req_body` — measured 2026-08-24

**Not a leak, and not urgent.** Recording it so nobody re-derives it.

| | |
|---|---|
| database | 5,914 MB, `freelist_count` **0** — no unreclaimed pages, it is all live data |
| `activity` table | 5,635 MB (95% of the file) |
| `req_body` column | **5,330 MB (90% of the file)**; `resp_body` is 242 MB |
| retention | **working** — oldest row is 29.7 days old against a 30 d setting. It has plateaued, not grown |
| disk | 380 GB free of 1.8 T |

**The cap is 256 KiB, not 4 KiB.** `payloadCap` (4 KiB) bounds a captured RESPONSE;
`reqBodyCap` is `256<<10`, raised deliberately so a full agentic request — system prompt + tool
schemas + multi-turn history + tool results — stays VALID JSON and can be replayed in the
console. A 4 KiB truncation left it unparseable and replay degraded to dumping raw text.
Overridable with `CORRALLM_REQBODY_CAP`.

**Where the bytes are:** 23,669 requests in the 64 KiB–1 MiB bucket account for 4,718 MB, and
the largest is 262,172 bytes — exactly the cap. These are agentic turns, where the same
preamble is re-sent every turn and stored in full every time. `dun` on `Qwen3-6-27B-MPT` alone
is 11,391 requests / 2,200 MB. (Traffic splits across `Qwen3-6-27B-MPT` and `local-Qwen3.8-27B`
because activity logs the REQUESTED name and the legacy spelling is an alias — expected, and
already recorded under P22.)

**Measured, not guessed:** per-row gzip on real payloads is **3.8×** — 5,330 MB would become
~1,400 MB. Compressing 200 concatenated rows instead gives 254×, which is not an achievable
column-encoder number but does say where the redundancy lives: **between** rows, not within
them. The same system prompt and tool schemas are stored thousands of times. The proxy already
knows this — P21's cache work measured a 72.3% prompt-cache hit rate and 231.6M tokens served
from cache.

**✅ RESOLVED — payload aging shipped (2026-08-24), 7 days by default.**
`--payload-retention` (`CORRALLM_PAYLOAD_RETENTION`, default 168h, 0 disables) clears
`req_body`/`resp_body` on rows older than the cutoff and KEEPS the row with every metric on it.
Runs in the same 5-minute maintenance tick as the row prune. Full tree in
[`done.md`](done.md) § payload aging.

**Verified against a real copy of the production database**, not a fixture: 57,205 rows cleared,
payloads 5,573 MB → 779 MB, and every aggregate byte-identical — rows 65,617, cost $6.276954,
prompt tokens 1,222,460,967, dwell 522,830,856 ms, cached tokens 1,028,349,212. Second pass
cleared 0 in 72 ms.

**✅ VACUUM run on the live database 2026-08-24** during the same maintenance window as the
deploy, with the daemon stopped so nothing contended for the lock. **5,916 MB → 803 MB**, 6.4 s,
`pragma integrity_check` = ok, and every aggregate identical across the rewrite (rows 65,617,
cost $6.276954, prompt tokens 1,222,460,967, dwell 522,830,856 ms).

`auto_vacuum` stays off, which is the right default: from here the payload prune keeps the file
flat by returning pages to the freelist for reuse, and a full rewrite is only ever needed after
a one-off backlog like this one.

Not taken: compressing the column (~3.8× per row, measured) — the aging removes the same bytes
without a format change or a decompress on read.

### Open decisions the USER owns

**They live in [`plan/open-questions.md`](open-questions.md), and there are four.**
(This table said "three" and listed three while §5 sat unlisted below it — corrected
2026-09-23.)

| # | question | gates | urgency |
|---|---|---|---|
| 1 | Agent lease: self-reap on/off, and its TTL | `host.Remote` (§6) | not yet — answer before that step |
| 2 | Is an *honest* wait estimate even wanted? | the wait-estimate formula (§6) | not yet — gather a wider traffic mix first |
| 4 | Should aw4 ride the `chat` lane? | nothing — the attribution fix is deployed and verified | whenever you like |
| 5 | The remap out: four calls corrallm may not guess (translations, image `url`, realtime format, and whether a model may declare that it diarizes) | P35 contracts 3, 4 and 5 — all unbuilt | not yet — contracts 1 and 2 shipped without them |

§3 (what a CALLER sees) closed 2026-09-11: **closed**, because the dashboard is admin-only and a
caller cannot open it. Answer and evidence in §6.

**Six others were closed on 2026-08-24 by checking the box instead of re-reading the plan.**
The answers and their evidence are recorded in the §6 slices that needed them, not here:
secrets location (P21c), the `discover:` filter (P21c — moot, already deployed), the KLD
reference (P22), gpu1 residency (VRAM slice — settled by 7 days of usage), transport trust
(multi-node — already answered in `wire.go`), and comfort-fill (P9f — never a decision;
it moved to `icebox.md`). P19's thermal tree closed with them and archived to `done.md`.
One known gap closed the same way: the upstream ~120 s timeout is no longer observable.

**The lesson worth keeping.** Six of eight "decisions" had already answered themselves — the
power cap was applied and persistent, the config had migrated, the trust position was written in
the code, the usage data made a four-way policy call a non-contest. A decision list that is not
re-checked against the running system becomes a list of questions nobody needs answered, and it
hides the two that are real.

---

## 8. Deployment (production cutover)

corrallm **replaced the llama-swap fork on `:8111`** for the live workload.

> **CORRECTION (2026-08-14).** The live config is **`~/.corrallm/config.yml`**, not
> `ml-kit/corrallm.yaml`. ml-kit was refactored to "the llama.cpp builder, nothing else"
> (`b72e7f0`) and no longer holds the deployment. The running command is authority:
> `corrallm serve --config ~/.corrallm/config.yml --web-root <repo>/ui/dist ...`.
> Everything below still describes the deployment correctly except that path.

The deployment lives outside this code repo:
- **the production config** — translated from `ml-kit/llama-swap.yaml`:
  two models (`nomic-embed-text` persistent/preloaded; `Qwen3-6-27B-MPT` sticky), absolute
  llama-server paths, fixed ports (5800/5801), fairshare groups (`aw3`→interactive=10,
  `ragtag`→batch=1, default=5), `scheduler.maxWait 60s`/`maxQueueDepth 8`. Pool budget reflects the
  real RTX 5090 (~32GB): Qwen `gpu0 29.5GB` + nomic `gpu0 1.5GB` (nomic offloads to GPU despite no
  `-ngl`). `commandCosts` are calibrated per type — `chat` (Qwen) vs `embed` (nomic), measured on the
  5090 (§7 gap 2); `/api` is gated by the `home/admin.token` admin token.
- **`ml-kit/bin/run`** — adapted from the llama-swap launcher: builds corrallm fresh from this repo
  (`go build` → repo `bin/corrallm`, gitignored), frees `:8111`, runs `serve` with
  `--health-timeout 600s` (matches llama-swap; Qwen's 220k-ctx cold load is ~66s). Supports
  `--detach` (setsid + `tmp/corrallm.pid`/`tmp/corrallm.log`; stop via `kill -- -$(cat tmp/corrallm.pid)`).
- The dashboard is fronted at **`https://llm.iodesystems.com`** (reverse proxy); SSE verified flowing
  through it (no buffering).
- **Build/run model:** corrallm is build-once-run (no hot-reload — `air` would thrash spawned model
  backends). UI changes are served from `ui/dist` per-request (browser reload picks them up unless a
  new GraphQL op needs the new binary); backend changes need a `bin/run` rebuild+restart.
- **Restart drill:** stop (`kill -- -<pid>`), wait for `:8111`/5800/5801 to free (~10s graceful reap),
  then `bin/run --detach`. Blip: in-flight requests drop, Qwen cold-reloads (~66s).
- **DB / retention:** SQLite at `ml-kit/local/corrallm.db`. Activity is pruned to 30d
  (`--activity-retention`); `lane_samples` to 48h. After the cost calibration, historical `cost_usd`
  was recomputed in place from stored tokens × the new `chat`/`embed` coefficients (one-time backfill,
  stop → backup → `UPDATE` → restart) so the 24h dashboard wasn't stuck on pre-calibration totals.

---

## 9. Capacity-parked (relocated from `~/inflight` 2026-08-04, and again 2026-08-10)

Three items that were sitting on the global shelf because they wait on a machine,
not on a decision. They belong here — the work and the config are both corrallm's.
Resume conditions are kept verbatim so nothing has to be re-derived.

### ⏸ Deploy the hardened Qwen chat template
**waits-on:** systems — the llm host must be quiet.
**check:** `[ "$(nvidia-smi --query-gpu=utilization.gpu --format=csv,noheader,nounits -i 1)" -lt 20 ]`
(exit 0 = ready. Samples ONE instant — observed flipping ready→busy within minutes.)

> **⚠ This check is index-based, and P18 is the reason to distrust that.** `-i 1` names a
> *position*, and the whole finding of P18 was that the nvidia-smi index MOVED onto the new card
> when the 3080 went in — "index is a fine label and a terrible identity". Qwen serves from gpu0.
> Confirm which card `-i 1` is before trusting a ready/busy answer from it, or re-express the
> check against the UUID the pools bind to.

**Check replaced 2026-08-10, because the old one failed CLOSED.** It polled
`http://127.0.0.1:5800/slots`. The port is right — corrallm spawns llama-server
on fixed slots 5800/5801 — but when nothing is bound there, `curl -sf` exits
non-zero on a refused connection and the pipeline reports NOT READY. The idlest
state, no model loaded at all, read as blocked. Observed that day: 5801 held an
embeddings model, 5800 was unbound. corrallm's own `/api/v1/active` on 8111 is
not a substitute — it answers 401, admin token required, and a token does not
belong in a versioned plan.

The served template renders untrusted tool-result content raw, so bytes from any
file an agent reads can inject a chat turn. **Not deployed:** no
`chat-template-file` in `corrallm.yaml` or `local/corrallm.yaml` as of 2026-08-04.

**Evidence (verified, do not re-derive):**
- `<|im_start|>` in content is **1 token**, not 6 — a real frame break, not a lookalike.
- Through **minja** (llama.cpp's own engine, `build/bin/test-chat-template`): stock
  yields turns `[user, assistant, user, SYSTEM, user, assistant]`; hardened yields
  `[user, assistant, user, assistant]`.
- The stock template is byte-identical to the live server's `/props` output — it is
  what actually runs.
- Hardened is a 4-line functional derivative; every structural literal preserved.

**canonical source:** `~/local/src/iodesystems/tool-call-lightly/templates/`, with an
offline suite (`./render-test`) that proves the fix through minja with no model. The
ml-kit copies are a stale duplicate — prefer the project.

**next:** 1) `corrallm features Qwen3-6-27B-MPT` for a baseline · 2) add
`--chat-template-file .../qwen3-27b-heredoc.jinja` to the Qwen entry in
`corrallm.yaml`, restart · 3) `corrallm features` again — **if round-trip fidelity
moved, the swap broke the autoparser: revert** · 4) `probe.py render`.

**risks:** llama.cpp's autoparser derives its parser FROM the template, so a template
change can silently break tool-call parsing. Step 3 is the guard, not a formality.

**UPDATE 2026-08-04 — llama.cpp rebuilt from master (b10273 / `4308a4f03`), and this
changes the risk.** `f5919bf45 chat : add qwen3 specialized parser (#26252)` (2026-08-02)
adds `common_chat_params_init_qwen3_coder`, selected by SNIFFING the template source for
all three of `<tool_call>`, `<function=`, `<parameter=`.

- **No Qwen template file changed upstream** — `models/templates/*Qwen*` is untouched
  across the whole 260-commit range. The change is parse-side, not render-side.
- **All four of our templates match all three literals** — `qwen3-stock`,
  `qwen3-hardened`, `qwen3-27b-stock`, `qwen3-27b-heredoc`. So stock and hardened now
  select the SAME specialized parser. **The risk above shrinks**: swapping templates no
  longer moves you onto a differently-derived parser. Step 3 stays the guard, but the
  failure mode is narrower than when this was written.
- **Every fidelity baseline is now stale as a comparison point.** The
  `corrallm features Qwen3-6-27B-MPT` numbers predate this parser. Re-baseline in
  step 1 rather than comparing against anything recorded earlier.
- **Open question:** the parser is labelled "Qwen3-Coder XML tool calls". Qwen3-6-27B-MPT
  is not a Coder model — it is selected purely by literal sniffing, so this may be a
  MISDETECTION. Its grammar is deliberately lenient in ways that matter (accepts required
  arguments in any order; "Qwen3-Coder models may occasionally omit the `<tool_call>`
  token"). Worth confirming the detection is intended for this model before deploying.

### ✅ Muse Glimmer 30B vs Qwen3-6-27B-MPT — the probe suite (relocated 2026-08-10) → closed 2026-10-02, [`done.md`](done.md)

### ✅ Second compute host: 64 GB MacBook Pro — ENROLLED 2026-08-04 → closed 2026-10-02, [`done.md`](done.md)

