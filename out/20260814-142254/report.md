# llm-bench report — 20260814-142254


## Class scores (-1 harmful · 0 incapable · +1 capable)

Score is the **baseline** arm — the model's own capability, unmoved by which tools happened to run. Tool arms are deltas against it: positive means the tool helped.

| model | class | score | | probes | weight | harmful | tool arms |
|---|---|---:|---|---:|---:|---:|---|
| Qwen3.8-27B | tooluse | +1.00 | capable | 1 | 1.0 |  | mcpshell +0.00; polylsp -1.00 |

A delta needs a MODEL SPREAD to mean anything: one model cannot show whether a tool helps generally or helped this one. See the arm matrix for the per-model view.
## Rollup (per model × toolset)

| model | toolset | stage pass % | bait | inv-arg | json-err | prompt tok | compl tok | avg tok/s |
|---|---|---:|---:|---:|---:|---:|---:|---:|
| Qwen3.8-27B | baseline | 100% (1/1) | 0 | 0.000 | 0 | 538434 | 1130 | 12914.7 |
| Qwen3.8-27B | mcpshell | 100% (1/1) | 0 | 0.000 | 0 | 538548 | 1492 | 13018.7 |
| Qwen3.8-27B | polylsp | 0% (0/1) | 0 | 0.000 | 0 | 188594 | 2066 | 5566.1 |

## Stage grid (per task)

### find-render-entrypoints (tooluse)

| model | toolset | stage | result | checks | bait | inv | json | rep | comp | ptok | ctok | ms |
|---|---|---:|---|---|---:|---:|---:|---:|---:|---:|---:|---:|
| Qwen3.8-27B | baseline | 0 | PASS | 4/4 | 0 | 0 | 0 | 1 | 0 | 538434 | 1130 | 41779 |
| Qwen3.8-27B | polylsp | 0 | FAIL* | 0/4 | 0 | 0 | 0 | 0 | 0 | 188594 | 2066 | 34254 |
| Qwen3.8-27B | mcpshell | 0 | PASS | 4/4 | 0 | 0 | 0 | 0 | 0 | 538548 | 1492 | 41482 |


`FAIL*` = stage aborted on a per-stage limit (turns or tool-call budget). `inv` = valid-JSON/wrong-shape tool args; `json` = malformed tool-call JSON output; `comp` = agentkit Shaper full-history compactions (LOD truncations are render-time and not reported). `retries429` is reserved (agentkit handles 429 internally with no hook — always 0 in P0). `judge`/`judge_quality` are reserved for P1 and are always null.
