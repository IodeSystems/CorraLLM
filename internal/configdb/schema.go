// Package configdb keeps corrallm's configuration in SQLite instead of a file.
//
// WHY IT MOVED. config.yml was already machine-owned — its own header said hand
// edits are not preserved, because the daemon rewrites it whenever anything
// changes. A struct-marshalled file that a program rewrites is a database with a
// bad storage engine, and it failed twice in ways a database cannot:
//
//   - It silently DELETED a section. A `tools:` block was added while a daemon
//     built before that field was running. Reading was safe (yaml.Unmarshal
//     ignores unknown fields) so the change looked inert, but the writer
//     marshals the in-memory struct, and a field the binary has no member for
//     has nowhere to live. The next write dropped the block entirely.
//   - It cannot hold an explanation. Comments die at the next rewrite, which is
//     why documentation had to be moved into `notes:` fields — the only prose a
//     marshaller preserves.
//
// `include:` goes with it. That mechanism exists solely to work around the file
// being rewritten ("a generated file included from here is machine-owned end to
// end, and the hand-written file stays hand-written"). With no file, there is
// nothing to protect from the writer.
//
// SHAPE. Normalized: real columns for anything queried, filtered or validated.
// Not a table per YAML nesting level — a few sub-objects are genuinely
// variable-shaped, and normalizing them would buy a join and lose nothing. The
// rule is a column when something joins or validates on it, JSON when the value
// is only ever carried along with its parent (see the notes on each such column
// below).
package configdb

// schema is applied idempotently, the same way internal/store does it.
//
// Every table carries the config it describes and nothing else: no ids that
// outlive a rewrite, no timestamps. Config is a SET, and the tables hold the
// current one. History, when it comes, is a separate concern layered above —
// putting a revision column on every table here would make each read filter on
// it and each write bump it, for a feature nothing has asked for yet.
const schema = `
CREATE TABLE IF NOT EXISTS config_scalar (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL          -- JSON; scalars and the small fixed singletons
);

CREATE TABLE IF NOT EXISTS config_server (
    name              TEXT PRIMARY KEY,
    max_concurrent    INTEGER NOT NULL DEFAULT 0,
    device_pool       TEXT    NOT NULL DEFAULT '',
    no_process_memory INTEGER NOT NULL DEFAULT 0,
    notes             TEXT    NOT NULL DEFAULT '',
    agent_json        TEXT    NOT NULL DEFAULT ''   -- endpoints + token; a whole object
);

-- The capacity vector, one row per pool. A column rather than JSON because
-- admission arithmetic is per-pool and the dashboard renders them individually.
CREATE TABLE IF NOT EXISTS config_server_pool (
    server  TEXT NOT NULL,
    pool    TEXT NOT NULL,
    size    TEXT NOT NULL DEFAULT '',   -- as written ("24GB"), parsed by config
    reserve TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (server, pool)
);

-- pool -> the physical card it budgets, by UUID or PCI id. Its own table
-- because a selector must be unique per server, which is a constraint a JSON
-- blob cannot express.
CREATE TABLE IF NOT EXISTS config_server_device (
    server   TEXT NOT NULL,
    pool     TEXT NOT NULL,
    selector TEXT NOT NULL,
    PRIMARY KEY (server, pool)
);

-- A model belongs to a provider. '' is the retired top-level shape, kept so an
-- old config still imports rather than being silently dropped.
CREATE TABLE IF NOT EXISTS config_model (
    provider            TEXT NOT NULL DEFAULT '',
    name                TEXT NOT NULL,
    type                TEXT NOT NULL DEFAULT '',
    quality             REAL NOT NULL DEFAULT 0,
    cmd                 TEXT NOT NULL DEFAULT '',
    server              TEXT NOT NULL DEFAULT '',
    upstream            TEXT NOT NULL DEFAULT '',
    max_concurrent      INTEGER NOT NULL DEFAULT 0,
    context_per_request INTEGER NOT NULL DEFAULT 0,
    persistent          INTEGER NOT NULL DEFAULT 0,
    notes               TEXT NOT NULL DEFAULT '',
    proxy_json          TEXT NOT NULL DEFAULT '',  -- port | host:port | {host,port,headers}
    aliases_json        TEXT NOT NULL DEFAULT '',
    -- Carried whole, never queried across: each is a small fixed-shape object
    -- read and written with its model.
    sticky_json         TEXT NOT NULL DEFAULT '',
    swap_json           TEXT NOT NULL DEFAULT '',
    ram_usage_json      TEXT NOT NULL DEFAULT '',
    modalities_json     TEXT NOT NULL DEFAULT '',
    convert_json        TEXT NOT NULL DEFAULT '',
    limits_json         TEXT NOT NULL DEFAULT '',
    placements_json     TEXT NOT NULL DEFAULT '',
    -- sampling is a map of named profiles whose knobs grow whenever llama.cpp
    -- adds one. A table would need a migration per sampler parameter.
    sampling_json       TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (provider, name)
);

CREATE TABLE IF NOT EXISTS config_lane (
    name  TEXT PRIMARY KEY,
    notes TEXT NOT NULL DEFAULT ''
);

-- Members are ORDERED — a lane is a fallback list, best first — so position is
-- part of the key rather than an implicit row order.
CREATE TABLE IF NOT EXISTS config_lane_member (
    lane        TEXT    NOT NULL,
    position    INTEGER NOT NULL,
    model       TEXT    NOT NULL DEFAULT '',
    provider    TEXT    NOT NULL DEFAULT '',  -- a provider selector, not a name
    sticky_json TEXT    NOT NULL DEFAULT '',  -- per-member override
    PRIMARY KEY (lane, position)
);

CREATE TABLE IF NOT EXISTS config_group (
    name            TEXT PRIMARY KEY,
    weight          INTEGER NOT NULL DEFAULT 0,
    share_currency  TEXT    NOT NULL DEFAULT '',
    interruptible   INTEGER NOT NULL DEFAULT 0,
    accept_degrade  INTEGER NOT NULL DEFAULT 0,
    quality_floor   REAL    NOT NULL DEFAULT 0,
    prefer_resident INTEGER NOT NULL DEFAULT 0,
    on_saturated_json TEXT  NOT NULL DEFAULT '',  -- backend type -> stage policy
    limits_json     TEXT    NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS config_key (
    key        TEXT PRIMARY KEY,
    group_name TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS config_tool (
    name    TEXT PRIMARY KEY,
    url     TEXT NOT NULL DEFAULT '',
    ref     TEXT NOT NULL DEFAULT '',
    recipe  TEXT NOT NULL DEFAULT '',
    bin     TEXT NOT NULL DEFAULT '',
    check_  TEXT NOT NULL DEFAULT '',   -- "check" is fine in sqlite but reads badly
    rebuild INTEGER NOT NULL DEFAULT 0,
    notes   TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS config_tool_host (
    tool         TEXT NOT NULL,
    host         TEXT NOT NULL,
    installed_at TEXT NOT NULL DEFAULT '',
    prefix       TEXT NOT NULL DEFAULT '',
    notes        TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (tool, host)
);

CREATE TABLE IF NOT EXISTS config_extension (
    name         TEXT PRIMARY KEY,
    cmd          TEXT NOT NULL DEFAULT '',
    server       TEXT NOT NULL DEFAULT '',
    notes        TEXT NOT NULL DEFAULT '',
    proxy_json   TEXT NOT NULL DEFAULT '',
    provides_json TEXT NOT NULL DEFAULT '',
    virtual_json TEXT NOT NULL DEFAULT '',   -- the pool definition, when virtual
    ram_usage_json TEXT NOT NULL DEFAULT '',
    sticky_json  TEXT NOT NULL DEFAULT '',
    rest_json    TEXT NOT NULL DEFAULT ''    -- fields with no column yet; see roundTrip
);

CREATE TABLE IF NOT EXISTS config_provider (
    extension  TEXT NOT NULL,
    name       TEXT NOT NULL,
    host       TEXT NOT NULL DEFAULT '',
    port       TEXT NOT NULL DEFAULT '',
    base_path  TEXT NOT NULL DEFAULT '',
    manual     INTEGER NOT NULL DEFAULT 0,
    rest_json  TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (extension, name)
);

CREATE TABLE IF NOT EXISTS config_local_provider (
    name            TEXT PRIMARY KEY,
    bare_precedence TEXT NOT NULL DEFAULT '',  -- nullable int; '' means unset
    notes           TEXT NOT NULL DEFAULT ''
);
`
