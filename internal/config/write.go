package config

// managedHeader tops every file corrallm writes.
//
// Blunt on purpose: the whole point of a machine-owned config is that nobody
// hand-edits it and then loses the edit on the next write. Saying so in the
// file is cheaper than discovering it.
const managedHeader = `# corrallm — MANAGED CONFIG. Edited through the dashboard or the API.
#
# Hand edits are NOT preserved: this file is rewritten whenever configuration
# changes, and a marshaller cannot keep YAML comments. Use the "notes" field on
# a model, server or lane for anything you want to write down — those are part
# of the config, survive rewriting, and are shown next to the entry they
# describe.
`

// forWriting returns a copy of c with DERIVED state removed.
//
// Load is not a pure parse: resolveExtensions expands every extension's
// `provides` into Models, overlaying the extension's cmd/server/proxy so the
// rest of the system sees ordinary spawned models. Those entries are not
// authored — they are computed — and marshalling them back out emits both the
// extension AND its expansion, so the next Load fails with "provided model
// collides with a declared model".
//
// In other words a Config that came from Load is a different shape to one that
// goes into Load, and writing without accounting for that produces a file the
// daemon cannot start from.
// ForWriting is forWriting, exported for the SQLite store.
//
// The rule is identical wherever config is persisted: store what was AUTHORED,
// never what resolution derived from it. A resolved config carries every
// extension-provided and provider-folded model in Models, and storing those
// makes the next load fail on "collides with a declared model" — the trap this
// function exists to avoid, which the database hit the first time it tried to
// save a config that had already been through Load.
func ForWriting(c *Config) *Config { return forWriting(c) }

// forWriting returns a copy of c with DERIVED state removed.
//
// Load is not a pure parse: resolveExtensions expands every extension's
// `provides` into Models, overlaying the extension's cmd/server/proxy so the
// rest of the system sees ordinary spawned models. Those entries are not
// authored — they are computed — and marshalling them back out emits both the
// extension AND its expansion, so the next Load fails with "provided model
// collides with a declared model".
//
// In other words a Config that came from Load is a different shape to one that
// goes into Load, and writing without accounting for that produces a file the
// daemon cannot start from.
func forWriting(c *Config) *Config {
	out := *c
	if len(c.Models) > 0 {
		models := make(map[string]Model, len(c.Models))
		for name, m := range c.Models {
			if m.Extension != "" {
				continue // derived from an extension; the extension is the source
			}
			// Same argument for a top-level provider's models: foldLocalProviders
			// puts them here under their prefixed names, but `providers:` is
			// where they were authored. Emitting both makes the next Load fail
			// on "collides with a model declared elsewhere" — the identical trap
			// extensions already sprang once.
			if _, fromProvider := c.Providers[m.ProviderName]; fromProvider {
				continue
			}
			models[name] = m
		}
		out.Models = models
	}
	return &out
}

// Everything that WROTE a config file is gone: Save, SaveValidated, and the
// comment-carrying ImportComments that existed to migrate a hand-written file
// into a managed one.
//
// Configuration lives in SQLite (P26). Nothing reads config.yml, so nothing
// should write one — a writer with no reader is a trap: it succeeds, and the
// daemon carries on ignoring the file it just produced. `corrallm config
// export` renders YAML for a human, and `config load` parses one back in.
//
// forWriting stays, and is the reason this file does: BOTH persistence paths
// need "store what was authored, not what resolution derived", and having one
// implementation of that rule is what keeps them agreeing.
