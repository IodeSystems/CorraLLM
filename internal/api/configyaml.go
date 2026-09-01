package api

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"gopkg.in/yaml.v3"

	"github.com/iodesystems/corrallm/internal/config"
)

// Editing config as YAML rather than through a form.
//
// A form can only expose the fields someone thought to add to it, and a model
// carries far more than fits one: ramUsage, sticky, contextPerRequest,
// modalities, convert, swap, freeTier. Every field the form omits is a field
// you cannot set from the dashboard, and every field added to the schema is one
// the form silently drops until someone updates it too.
//
// YAML is the schema. Editing it directly means the editor is complete the day
// a field is added, and the round-trip through Load/Validate gives exactly the
// same guarantees the file on disk has — with the error attached to the edit
// instead of discovered at the next restart.

// redactedToken is what a stored agent token is replaced with on the way out.
//
// The dashboard already requires the admin token, so showing it is not an
// escalation — but a credential that appears on screen gets copied into
// screenshots and pasted into chats, and there is no reason for it to leave the
// server. On save the placeholder is swapped back for the stored value, so
// editing a server's pools cannot silently revoke its agent.
const redactedToken = "<unchanged>"

// EntryYAMLInput fetches one config entry as YAML.
type EntryYAMLInput struct {
	Kind string `path:"kind" doc:"model | server | lane | group | extension | tool | key"`
	Name string `path:"name"`
}

// EntryYAMLOutput carries the YAML body.
type EntryYAMLOutput struct {
	Body struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
		YAML string `json:"yaml" doc:"The entry exactly as stored, with any secret redacted."`
	}
}

// EntryYAML returns one model, server or lane as YAML for editing.
func (h *Handlers) EntryYAML(_ context.Context, in *EntryYAMLInput) (*EntryYAMLOutput, error) {
	cfg := h.config()
	if cfg == nil {
		return nil, huma.Error503ServiceUnavailable("config unavailable")
	}
	var v any
	switch in.Kind {
	case "model":
		m, ok := cfg.Models[in.Name]
		if !ok {
			return nil, huma.Error404NotFound(fmt.Sprintf("no model %q", in.Name))
		}
		v = m
	case "server":
		srv, ok := cfg.Servers[in.Name]
		if !ok {
			return nil, huma.Error404NotFound(fmt.Sprintf("no server %q", in.Name))
		}
		if srv.Agent != nil && srv.Agent.Token != "" {
			// Copy before redacting: Server holds a POINTER to the binding, so
			// blanking it in place would wipe the live config's token.
			b := *srv.Agent
			b.Token = redactedToken
			srv.Agent = &b
		}
		v = srv
	case "lane":
		l, ok := cfg.Lanes[in.Name]
		if !ok {
			return nil, huma.Error404NotFound(fmt.Sprintf("no lane %q", in.Name))
		}
		v = l
	case "group":
		g, ok := cfg.PriorityGroups[in.Name]
		if !ok {
			return nil, huma.Error404NotFound(fmt.Sprintf("no priority group %q", in.Name))
		}
		v = g
	case "extension":
		e, ok := cfg.Extensions[in.Name]
		if !ok {
			return nil, huma.Error404NotFound(fmt.Sprintf("no extension %q", in.Name))
		}
		v = e
	case "tool":
		t, ok := cfg.Tools[in.Name]
		if !ok {
			return nil, huma.Error404NotFound(fmt.Sprintf("no tool %q", in.Name))
		}
		v = t
	case "key":
		// A caller key's whole entry is the GROUP it belongs to, so its YAML is
		// a bare string. Keys are the one part of the scheduling model with no
		// management surface at all — every other kind is editable here while
		// key→group needed a hand-edit and a restart, and it is the part that
		// changes most, because keys are minted freely.
		//
		// Addressed by KEY, not by hash: the hash exists to give the UI a
		// stable display identifier, and resolving it back would make this
		// endpoint depend on a lookup that only holds while the key is
		// configured — useless for ASSIGNING a key that is not yet in the map.
		g, ok := cfg.Keys[in.Name]
		if !ok {
			return nil, huma.Error404NotFound(fmt.Sprintf("no caller key %q", in.Name))
		}
		v = g
	default:
		return nil, huma.Error400BadRequest("kind must be model, server, lane, group, extension, tool or key")
	}
	b, err := yaml.Marshal(v)
	if err != nil {
		return nil, huma.Error500InternalServerError("could not render the entry", err)
	}
	out := &EntryYAMLOutput{}
	out.Body.Kind, out.Body.Name, out.Body.YAML = in.Kind, in.Name, string(b)
	return out, nil
}

// PutEntryYAMLInput replaces one entry from YAML.
type PutEntryYAMLInput struct {
	Kind string `path:"kind" doc:"model | server | lane | group | extension | tool | key"`
	Name string `path:"name"`
	Body struct {
		YAML string `json:"yaml"`
	}
}

// PutEntryYAML parses, validates and applies a model, server or lane.
func (h *Handlers) PutEntryYAML(_ context.Context, in *PutEntryYAMLInput) (*ConfigMutationOutput, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, huma.Error400BadRequest("an entry needs a name")
	}
	decode := func(v any) error {
		dec := yaml.NewDecoder(strings.NewReader(in.Body.YAML))
		dec.KnownFields(true) // a typo'd key must fail, not vanish
		if err := dec.Decode(v); err != nil {
			return huma.Error400BadRequest(fmt.Sprintf("invalid YAML: %v", err))
		}
		return nil
	}

	err := h.mutateConfig(func(c *config.Config) error {
		switch in.Kind {
		case "model":
			var m config.Model
			if err := decode(&m); err != nil {
				return err
			}
			if prev, ok := c.Models[name]; ok && prev.Extension != "" {
				return huma.Error409Conflict(fmt.Sprintf(
					"%q is provided by extension %q — edit the extension instead", name, prev.Extension))
			}
			c.Models[name] = m
		case "server":
			var srv config.Server
			if err := decode(&srv); err != nil {
				return err
			}
			// Restore a redacted token rather than writing the placeholder,
			// which would silently revoke the agent on the next heartbeat.
			if srv.Agent != nil && srv.Agent.Token == redactedToken {
				if prev, ok := c.Servers[name]; ok && prev.Agent != nil {
					srv.Agent.Token = prev.Agent.Token
				} else {
					srv.Agent.Token = ""
				}
			}
			c.Servers[name] = srv
		case "lane":
			var l config.Lane
			if err := decode(&l); err != nil {
				return err
			}
			c.Lanes[name] = l
		case "group":
			var g config.PriorityGroup
			if err := decode(&g); err != nil {
				return err
			}
			if c.PriorityGroups == nil {
				c.PriorityGroups = map[string]config.PriorityGroup{}
			}
			c.PriorityGroups[name] = g
		case "extension":
			var e config.Extension
			if err := decode(&e); err != nil {
				return err
			}
			if c.Extensions == nil {
				c.Extensions = map[string]config.Extension{}
			}
			c.Extensions[name] = e
			// An extension EXPANDS into models at load. The copy we are editing
			// still holds the previous expansion, and re-validating with both
			// present fails as a name collision — so drop them and let the
			// reload rebuild from the extension, which is their only source.
			for mn, m := range c.Models {
				if m.Extension == name {
					delete(c.Models, mn)
				}
			}
		case "tool":
			var t config.Tool
			if err := decode(&t); err != nil {
				return err
			}
			// Normalize here rather than only in ValidatePin: a sha pasted from
			// a UI arrives in whatever case it was copied in, and two spellings
			// of one commit that do not compare equal would make a pinned tool
			// report itself as behind its own pin forever.
			t.Pin = config.NormalizePin(t.Pin)
			if c.Tools == nil {
				c.Tools = map[string]config.Tool{}
			}
			c.Tools[name] = t
		case "key":
			// Both written forms, because KeyPolicy.UnmarshalYAML accepts both:
			// a bare group name, or a policy naming a default group plus the
			// groups this key may escalate into.
			var pol config.KeyPolicy
			if err := decode(&pol); err != nil {
				return err
			}
			pol.Group = strings.TrimSpace(pol.Group)
			if pol.Group == "" {
				return huma.Error400BadRequest(
					"a key entry is either a GROUP name, or a mapping with `default: <group>` " +
						"and `<group>: true` for each escalation it may request")
			}
			// Reject an unknown group rather than accepting it: ResolveCaller
			// falls back silently, so a typo would look like a successful
			// assignment and quietly leave the caller in the fallback lane at
			// weight 1 — the failure this endpoint exists to end.
			if _, ok := c.PriorityGroups[pol.Group]; !ok && pol.Group != c.UnknownKeys.FallbackGroup() {
				return huma.Error400BadRequest(fmt.Sprintf(
					"no priority group %q; assigning it would silently resolve to %q",
					pol.Group, c.UnknownKeys.FallbackGroup()))
			}
			// Same argument one level down: an escalation naming a group that
			// does not exist is refused at request time with nothing saying why,
			// so it is refused here instead, where someone is reading the error.
			for g := range pol.Allow {
				if _, ok := c.PriorityGroups[g]; !ok {
					return huma.Error400BadRequest(fmt.Sprintf(
						"key %q may not escalate to %q: no such priority group", name, g))
				}
			}
			if c.Keys == nil {
				c.Keys = map[string]config.KeyPolicy{}
			}
			// The bare-string form carries no escalations, and must not silently
			// REVOKE the ones a key already had — that endpoint sets where a key
			// lands. A mapping states the whole policy and replaces it.
			if len(pol.Allow) == 0 {
				pol.Allow = c.Keys[name].Allow
			}
			c.Keys[name] = pol
		default:
			return huma.Error400BadRequest("kind must be model, server, lane, group, extension, tool or key")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := &ConfigMutationOutput{}
	out.Body.OK = true
	out.Body.Message = fmt.Sprintf("saved %s %s", in.Kind, name)
	return out, nil
}

// DeleteEntryInput names what to remove.
type DeleteEntryInput struct {
	Kind string `path:"kind" doc:"model | server | lane | group | extension | tool | key"`
	Name string `path:"name"`
}

// DeleteEntry removes a model, server or lane, refusing when something still
// depends on it.
//
// Validation would catch most of this on save, but naming the dependants is far
// more useful than "unknown model" — it says what to fix rather than that
// something is wrong.
func (h *Handlers) DeleteEntry(_ context.Context, in *DeleteEntryInput) (*ConfigMutationOutput, error) {
	err := h.mutateConfig(func(c *config.Config) error {
		switch in.Kind {
		case "model":
			if _, ok := c.Models[in.Name]; !ok {
				return huma.Error404NotFound(fmt.Sprintf("no model %q", in.Name))
			}
			var used []string
			for lane, l := range c.Lanes {
				for _, mem := range l.Members {
					if mem.Model == in.Name {
						used = append(used, lane)
					}
				}
			}
			if len(used) > 0 {
				sort.Strings(used)
				return huma.Error409Conflict(fmt.Sprintf(
					"%q is a member of lane(s) %s — remove it there first", in.Name, strings.Join(used, ", ")))
			}
			// Delete it where it was AUTHORED. A model owned by a top-level
			// provider lives in providers.<p>.models; c.Models only holds the
			// folded copy, and forWriting deliberately drops that copy — so
			// deleting only from c.Models reported success and changed nothing
			// on disk, and the model came back on the next load.
			deleted := false
			for pn, lp := range c.Providers {
				for id := range lp.Models {
					if config.ServedName(pn, id) != in.Name {
						continue
					}
					delete(lp.Models, id)
					c.Providers[pn] = lp
					deleted = true
					break
				}
				if deleted {
					break
				}
			}
			// And a REMOTE provider's model, authored under
			// extensions.<ext>.providers.<p>.provides. Same argument, third
			// possible location — forWriting drops extension-derived models
			// from c.Models, so deleting only there left the provides entry
			// intact and the model returned on the next load.
			if !deleted {
				for en, ext := range c.Extensions {
					for pn, pv := range ext.Providers {
						for id := range pv.Provides {
							if config.ServedName(pn, id) != in.Name {
								continue
							}
							// A provider must contribute something. Deleting its
							// LAST declared model leaves an endpoint nothing can
							// reach, which config validation rejects — with a
							// message about provider shape rather than about the
							// delete that caused it. Say it here instead, and
							// name the ways out.
							if len(pv.Provides) == 1 && pv.Discover == nil && !pv.Manual && ext.Virtual == nil {
								return huma.Error409Conflict(fmt.Sprintf(
									"%q is the only model provider %q declares — deleting it would leave an endpoint nothing can reach. Delete the provider instead, or give it another way to contribute (choose models off its directory, or pool it in a virtual extension) first.",
									in.Name, pn))
							}
							delete(pv.Provides, id)
							ext.Providers[pn] = pv
							c.Extensions[en] = ext
							deleted = true
							break
						}
						if deleted {
							break
						}
					}
					if deleted {
						break
					}
				}
			}
			delete(c.Models, in.Name)
		case "server":
			if _, ok := c.Servers[in.Name]; !ok {
				return huma.Error404NotFound(fmt.Sprintf("no server %q", in.Name))
			}
			var on []string
			for m, mdl := range c.Models {
				if mdl.Server == in.Name {
					on = append(on, m)
				}
			}
			for e, ext := range c.Extensions {
				if ext.Server == in.Name {
					on = append(on, "extension "+e)
				}
			}
			if len(on) > 0 {
				sort.Strings(on)
				return huma.Error409Conflict(fmt.Sprintf(
					"%q still hosts %s — move or delete them first", in.Name, strings.Join(on, ", ")))
			}
			delete(c.Servers, in.Name)
		case "lane":
			if _, ok := c.Lanes[in.Name]; !ok {
				return huma.Error404NotFound(fmt.Sprintf("no lane %q", in.Name))
			}
			delete(c.Lanes, in.Name)
		case "group":
			if _, ok := c.PriorityGroups[in.Name]; !ok {
				return huma.Error404NotFound(fmt.Sprintf("no priority group %q", in.Name))
			}
			// A key that may ESCALATE into this group is using it too — deleting
			// the group would leave that permission naming nothing, which
			// validation then rejects on the next load.
			var keys []string
			for k, pol := range c.Keys {
				if pol.Group == in.Name || pol.Allow[in.Name] {
					keys = append(keys, k)
				}
			}
			if len(keys) > 0 {
				sort.Strings(keys)
				return huma.Error409Conflict(fmt.Sprintf(
					"%q is the group for key(s) %s — those callers would fall back to the default lane",
					in.Name, strings.Join(keys, ", ")))
			}
			delete(c.PriorityGroups, in.Name)
		case "extension":
			if _, ok := c.Extensions[in.Name]; !ok {
				return huma.Error404NotFound(fmt.Sprintf("no extension %q", in.Name))
			}
			// Its provided models go with it: they are one process, and an
			// expansion outliving its extension is a model nothing can spawn.
			for mn, m := range c.Models {
				if m.Extension == in.Name {
					delete(c.Models, mn)
				}
			}
			delete(c.Extensions, in.Name)
		case "tool":
			if _, ok := c.Tools[in.Name]; !ok {
				return huma.Error404NotFound(fmt.Sprintf("no tool %q", in.Name))
			}
			// A model whose cmd says ${tool:x} cannot spawn without x — the
			// expansion refuses rather than falling back to PATH, which is the
			// whole point of the binding. Name the models instead of letting
			// them fail at the next load.
			var used []string
			for mn, m := range c.Models {
				if strings.Contains(m.Cmd, "${tool:"+in.Name+"}") {
					used = append(used, mn)
				}
			}
			if len(used) > 0 {
				sort.Strings(used)
				return huma.Error409Conflict(fmt.Sprintf(
					"%q is referenced by model(s) %s — they would fail to spawn; repoint them first",
					in.Name, strings.Join(used, ", ")))
			}
			delete(c.Tools, in.Name)
		case "key":
			if _, ok := c.Keys[in.Name]; !ok {
				return huma.Error404NotFound(fmt.Sprintf("no caller key %q", in.Name))
			}
			// Unassigning, not revoking. corrallm accepts any key and resolves
			// an unknown one to "default", so a deleted key keeps working at
			// weight 1 — this drops its lane assignment, it does not lock the
			// caller out, and nothing here should imply otherwise.
			delete(c.Keys, in.Name)
		default:
			return huma.Error400BadRequest("kind must be model, server, lane, group, extension, tool or key")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := &ConfigMutationOutput{}
	out.Body.OK = true
	out.Body.Message = fmt.Sprintf("deleted %s %s", in.Kind, in.Name)
	return out, nil
}
