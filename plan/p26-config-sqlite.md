# P26 — config moves into SQLite

Status: **P26a in progress** (2026-08-18).

## 1. Why

`config.yml` is already a machine-owned file. Its own header says so: *"MANAGED
CONFIG. Edited through the dashboard or the API. Hand edits are NOT preserved:
this file is rewritten whenever configuration changes, and a marshaller cannot
keep YAML comments."* A struct-marshalled file that the daemon rewrites is a
database with a bad storage engine, and this session produced two proofs of
that:

- **It silently deleted a section.** A `tools:` block was added while a daemon
  built before that field was running. Reading was safe — `yaml.Unmarshal`
  ignores unknown fields — so the change looked inert. But `forWriting`
  marshals the in-memory struct, and a field the running binary has no member
  for has nowhere to live. The next autonomous write dropped the whole block.
- **It cannot hold an explanation.** Every comment written into that file dies
  at the next rewrite, which is why the tools documentation had to be moved into
  `notes:` fields — the only prose the marshaller preserves.

Plus `include:`, which exists solely to work around the file being rewritten:
"a generated file included from here is machine-owned end to end, and the
hand-written file stays hand-written". With no file, that whole mechanism goes.

## 2. Decisions (user, 2026-08-18)

| Decision | Choice |
|---|---|
| Shape | **Fully normalized tables** — real columns, not a blob |
| Authority | **SQLite**; YAML becomes export/import only |
| Cutover | **One-time import**, DB authoritative afterwards |
| The old file | **Deleted after a verified import** |

"Verified" is doing a lot of work in that last row, so it is defined here rather
than left to judgement: an import is verified when the config read back OUT of
the tables is *semantically equal* to the config parsed from the file — same
servers, models, lanes, groups, keys, tools, extensions, providers and scalars,
compared as parsed structures rather than as text. Nothing is deleted until that
comparison passes on the operator's real config, and a timestamped copy is
written beside the DB first regardless.

## 3. Shape

Fully normalized means real columns for everything that is queried, edited or
validated. It does NOT mean a table per YAML nesting level: a few sub-objects
are genuinely variable-shaped and normalizing them would buy a join and lose
nothing.

Normalized into columns:

```
config_scalar     (key, value)              -- costPerKwh, and the singletons
config_server     (name, max_concurrent, device_pool, notes, ...)
config_server_pool(server, pool, size, reserve)      -- the capacity vector
config_server_device(server, pool, selector)         -- pool -> physical card
config_model      (provider, name, type, quality, cmd, server, proxy, ...)
config_lane       (name, notes)
config_lane_member(lane, position, model, provider, sticky_json)
config_group      (name, weight, interruptible, share_currency, ...)
config_key        (key, group_name)
config_tool       (name, url, ref, recipe, bin, check, rebuild, notes)
config_tool_host  (tool, host, installed_at, prefix, notes)
config_extension  (name, cmd, server, virtual, notes, ...)
config_provider   (extension, name, host, port, base_path, ...)
config_credential (extension, provider, name, secret_ref, header_name, ...)
```

Kept as JSON columns, deliberately:

- `sampling` — a map of named profiles, each a bag of sampler knobs that grows
  whenever llama.cpp adds one. A table would need a migration per knob.
- `modalities`, `convert`, `swap`, `sticky`, `ramUsage`, `limits` — small
  fixed-shape sub-objects always read and written whole, never queried across.
- `commandCosts` — `map[string]map[string]any` by definition; its whole purpose
  is holding per-backend-type cost parameters nobody enumerated in advance.

The rule: a column when something filters, joins or validates on it; JSON when
it is only ever carried along with its parent.

## 4. Phases

- **P26a — schema + round trip, wired to nothing.** Write a `Config` into the
  tables, read it back, prove semantic equality against the LIVE config. Until
  this passes on the real thing, nothing else is safe to build.
- **P26b — `corrallm config export` / `import`.** YAML in, YAML out, against the
  DB. This is the escape hatch that makes deleting the file survivable.
- **P26c — the daemon reads from the DB.** Load prefers the tables; an empty DB
  with a file present triggers the one-time import. Writers (`configedit`,
  `enroll`) write tables.
- **P26d — retire the file.** Delete after verification, drop `include:`
  support, and make a stale `config.yml` a loud startup note rather than a
  silent no-op.
- **P26e — what the DB makes possible.** Per-entry writes (two operators editing
  different models stop clobbering each other) and config history.

## 5. Risks

- **The blast radius is everything.** Config is what the scheduler, proxy,
  residency and UI all read. P26a is deliberately inert for that reason.
- **A partial write leaves an inconsistent config**, which a single file could
  not do. Every write is one transaction.
- **Deleting the file removes the plain-text copy** on the same day the DB
  becomes the only one. Mitigated by export, by the pre-delete backup, and by
  refusing to delete unless the round trip passes.
- **Unknown/forward fields.** The file tolerated a field the binary did not know
  (it ignored it on read, and dropped it on write). Columns cannot: an unknown
  key becomes an explicit error at import instead of silent data loss. That is
  better, but it IS a behaviour change.
