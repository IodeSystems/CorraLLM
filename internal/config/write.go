package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

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

// Save writes c to path atomically, creating parent directories.
//
// Atomic because this file IS the daemon's configuration. A crash or a full
// disk partway through a naive write leaves a truncated YAML that fails to
// parse, and the next start has no config at all — losing every model on a
// restart is a far worse failure than the write not happening.
func Save(path string, c *Config) error {
	if c == nil {
		return fmt.Errorf("save config: nil config")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("save config: %w", err)
	}

	var buf bytes.Buffer
	buf.WriteString(managedHeader)
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(forWriting(c)); err != nil {
		return fmt.Errorf("save config: encode: %w", err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("save config: encode: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".corrallm-config-*")
	if err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once renamed

	if _, err := tmp.Write(buf.Bytes()); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("save config: %w", err)
	}
	// Durability before the rename: a rename is atomic, but the CONTENT it
	// points at is not on disk until synced, and a power loss between the two
	// leaves an atomically-renamed empty file.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("save config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	return os.Rename(tmpName, path)
}

// SaveValidated encodes, RE-PARSES and validates before replacing path.
//
// The round trip is the point. A caller can hand us an in-memory Config that
// serialises to something Load would reject — a lane naming a model that was
// just deleted, a devicePool that is not a pool — and writing it would leave a
// daemon that cannot restart. Proving the bytes load before they land makes a
// bad edit fail at the API instead of at 3am.
func SaveValidated(path string, c *Config) error {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(forWriting(c)); err != nil {
		return fmt.Errorf("save config: encode: %w", err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("save config: encode: %w", err)
	}
	if _, err := LoadBytesForTest(buf.Bytes()); err != nil {
		return fmt.Errorf("refusing to write an invalid config: %w", err)
	}
	return Save(path, c)
}

// ImportComments lifts the YAML comments of an existing file onto the Notes of
// the entries they describe.
//
// This is the migration's whole reason for being careful. A hand-written config
// is typically half commentary, and that commentary is where the operational
// knowledge lives — why a model is failover rather than a degrade tier, which
// build trap cost a day. Marshalling the parsed struct back out drops every one
// of those lines with no warning. yaml.v3 exposes them on the node tree, so the
// import can move them into a field that survives.
//
// Head comments (the block ABOVE a key) and foot comments are taken; a line
// comment trailing the key is appended. Comments that belong to no entry — a
// file header, a section banner — are unreachable this way and are returned
// separately so the caller can decide rather than silently lose them.
func ImportComments(src []byte, c *Config) (orphaned []string, err error) {
	var root yaml.Node
	if err := yaml.Unmarshal(src, &root); err != nil {
		return nil, fmt.Errorf("import comments: %w", err)
	}
	if len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return nil, nil
	}
	top := root.Content[0]

	// Section → the map we write Notes into. Only entities that have a Notes
	// field are covered; anything else contributes to orphaned.
	for i := 0; i+1 < len(top.Content); i += 2 {
		section, body := top.Content[i], top.Content[i+1]
		if body.Kind != yaml.MappingNode {
			if cm := comments(section); cm != "" {
				orphaned = append(orphaned, section.Value+": "+cm)
			}
			continue
		}
		// A comment on the SECTION itself (e.g. above `models:`) belongs to no
		// single entry.
		if cm := comments(section); cm != "" {
			orphaned = append(orphaned, section.Value+": "+cm)
		}
		for j := 0; j+1 < len(body.Content); j += 2 {
			key, val := body.Content[j], body.Content[j+1]
			// The entity's OWN comment plus everything written inside it.
			//
			// Most of the value is nested: the comment explaining why a context
			// window is what it is sits above `contextPerRequest`, not above
			// the model name. Taking only the head comment on the entity key
			// captured a one-line summary and silently dropped the paragraphs
			// that cost real debugging to learn.
			cm := joinNotes(comments(key), subtreeComments(val, ""))
			if cm == "" {
				continue
			}
			switch section.Value {
			case "models":
				if m, ok := c.Models[key.Value]; ok {
					m.Notes = joinNotes(m.Notes, cm)
					c.Models[key.Value] = m
					continue
				}
			case "servers":
				if s, ok := c.Servers[key.Value]; ok {
					s.Notes = joinNotes(s.Notes, cm)
					c.Servers[key.Value] = s
					continue
				}
			case "extensions":
				if e, ok := c.Extensions[key.Value]; ok {
					e.Notes = joinNotes(e.Notes, cm)
					c.Extensions[key.Value] = e
					continue
				}
			case "lanes":
				if l, ok := c.Lanes[key.Value]; ok {
					l.Notes = joinNotes(l.Notes, cm)
					c.Lanes[key.Value] = l
					continue
				}
			}
			orphaned = append(orphaned, section.Value+"."+key.Value+": "+cm)
		}
	}
	return orphaned, nil
}

// subtreeComments gathers every comment inside a node, labelled with the field
// it was attached to so the note still says what it is about once it is no
// longer positioned next to it.
func subtreeComments(n *yaml.Node, prefix string) string {
	if n == nil {
		return ""
	}
	var parts []string
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			path := k.Value
			if prefix != "" {
				path = prefix + "." + k.Value
			}
			if cm := comments(k); cm != "" {
				parts = append(parts, path+":\n"+cm)
			}
			if sub := subtreeComments(v, path); sub != "" {
				parts = append(parts, sub)
			}
		}
	case yaml.SequenceNode:
		for _, v := range n.Content {
			if cm := comments(v); cm != "" {
				parts = append(parts, prefix+":\n"+cm)
			}
			if sub := subtreeComments(v, prefix); sub != "" {
				parts = append(parts, sub)
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

// comments collects a node's head, line and foot comments into one block,
// stripped of the leading "# ".
func comments(n *yaml.Node) string {
	var parts []string
	for _, c := range []string{n.HeadComment, n.LineComment, n.FootComment} {
		if s := stripHashes(c); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "\n")
}

func stripHashes(s string) string {
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		l = strings.TrimSpace(l)
		l = strings.TrimPrefix(l, "#")
		out = append(out, strings.TrimSpace(l))
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

func joinNotes(existing, add string) string {
	if existing == "" {
		return add
	}
	if add == "" {
		return existing
	}
	return existing + "\n\n" + add
}
