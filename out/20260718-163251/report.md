# llm-bench report — 20260718-163251

## Rollup (per model × toolset)

| model | toolset | stage pass % | bait | inv-arg | json-err | prompt tok | compl tok | avg tok/s |
|---|---|---:|---:|---:|---:|---:|---:|---:|
| gemma-4-12b | polylsp | 0% (0/1) | 0 | 0.000 | 0 | 0 | 0 | 0.0 |
| gemma-4-12b | polylsp-validate | 0% (0/1) | 0 | 0.000 | 0 | 0 | 0 | 0.0 |

## Stage grid (per task)

### edit-safety-pop (coding)

| model | toolset | stage | result | checks | bait | inv | json | rep | comp | ptok | ctok | ms |
|---|---|---:|---|---|---:|---:|---:|---:|---:|---:|---:|---:|
| gemma-4-12b | polylsp-validate | 0 | FAIL | 0/3 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 0 |
| gemma-4-12b | polylsp | 0 | FAIL* | 2/3 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 0 |


`FAIL*` = stage aborted on a per-stage limit (turns or tool-call budget). `inv` = valid-JSON/wrong-shape tool args; `json` = malformed tool-call JSON output; `comp` = agentkit Shaper full-history compactions (LOD truncations are render-time and not reported). `retries429` is reserved (agentkit handles 429 internally with no hook — always 0 in P0). `judge`/`judge_quality` are reserved for P1 and are always null.
