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
	Kind string `path:"kind" doc:"model | server | lane"`
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
	default:
		return nil, huma.Error400BadRequest("kind must be model, server or lane")
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
	Kind string `path:"kind" doc:"model | server | lane"`
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
		default:
			return huma.Error400BadRequest("kind must be model, server or lane")
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
	Kind string `path:"kind" doc:"model | server | lane"`
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
		default:
			return huma.Error400BadRequest("kind must be model, server or lane")
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
