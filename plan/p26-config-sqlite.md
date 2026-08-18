# P26 — config moves into SQLite

Status: **COMPLETE and live** (2026-08-18). All five phases shipped; the
production daemon boots from SQLite and `~/.corrallm/config.yml` is retired.

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

## 4. Phases — all shipped

- **✅ P26a — schema + round trip** (`0646b92`). Normalized tables, wired to
  nothing. The mapper is LOSSLESS BY CONSTRUCTION: entities round-trip through
  their own YAML as a map, columns are lifted out, and the remainder is stored
  verbatim — so forgetting a field costs a column, not data. Proven against the
  real config, and the test was itself verified by dropping one field and
  watching it fail.
- **✅ P26b — export / load** (`2d2774b`). The escape hatch, landed before
  anything destructive. Tested as a FIXED POINT (export → import → export must
  be byte-identical), which catches a field surviving one direction only.
- **✅ P26c — the daemon reads from the DB** (`ea7472e`). Three named boot
  cases. SIGHUP reloads from the database; re-reading the file would have
  silently reverted every dashboard edit since startup.
- **✅ P26d — retire the file, drop `include:`** (`e18d7ed`). Renamed rather
  than deleted, because this is the moment the DB becomes the only copy.
- **✅ P26e — history and restore** (`a31b56d`). Every save records what the
  config became; restore is a change, not a rewind.

### What it cost, and what it caught

Four bugs, each found by a test or by running it rather than by reading:

1. **Double resolution.** Saving a config that had been through `config.Load`
   re-resolved it and reported every extension-provided model as colliding with
   itself. Fixed by reducing to the AUTHORED form first — the rule the file
   writer always applied, now shared via the exported `config.ForWriting`.
2. **A fresh install wrote a config file just to retire it.** `bootstrapConfig`
   existed so `requireManaged` would accept an edit; with the import path in
   place the daemon imported that empty file and retired it two log lines later.
   Gone entirely — a fresh install now creates `admin.token` and the DB, nothing
   else. Caught by `TestRawSpinup`.
3. **Restore double-recorded.** `Save` records and `Restore` recorded again, so
   every rollback appeared twice in its own history.
4. **`Apply` created the config tables but not the revision table.** Any Source
   built outside the daemon failed on its first save. They are one call now,
   because a Source that cannot record is broken.

### Live cutover (2026-08-18)

Imported, verified, retired to `config.yml.imported-20260818-162738`, booted
from the database: 2 servers, 15 models, 3 groups, 28 served. `Qwen3.8-27B`
generated from a DB-sourced config. Revision 1 reads "imported from
/home/nthalk/.corrallm/config.yml".

**Anything external that read `~/.corrallm/config.yml` now finds nothing.**
`corrallm config export` is the replacement.

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

## 6. Not done

- **Per-entry writes.** Two operators editing different models still read,
  modify and write the WHOLE config, so the later save wins. The tables support
  finer writes; nothing uses them yet. Low urgency at one operator.
- **History in the UI.** `config history | show | restore` is CLI-only. The
  dashboard has no view of it, which is where somebody would actually notice a
  config had changed under them.
- **`corrallm config import`** (the comment-carrying migration) still writes a
  FILE, which nothing reads any more. It should target the database or go.
