package api

import (
	"context"
	"fmt"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"gopkg.in/yaml.v3"

	"github.com/iodesystems/corrallm/internal/config"
)

// mutateConfig applies fn to a COPY of the live config, then validates, writes
// and reloads — or changes nothing at all.
//
// Copy-first is the whole safety property. Mutating the live config and then
// discovering it is invalid leaves the daemon running something that cannot be
// reloaded or restarted from, and the operator finds out at the worst possible
// moment. Here a rejected edit is a 400 and the running system is untouched.
func (h *Handlers) mutateConfig(fn func(*config.Config) error) error {
	if h.ConfigPath == "" {
		return huma.Error503ServiceUnavailable("this daemon has no writable config")
	}
	if err := requireManaged(h.ConfigPath); err != nil {
		return huma.Error409Conflict(err.Error())
	}
	cur := h.config()
	if cur == nil {
		return huma.Error503ServiceUnavailable("config unavailable")
	}
	next := copyForEdit(cur)
	if err := fn(next); err != nil {
		return err
	}
	if err := config.SaveValidated(h.ConfigPath, next); err != nil {
		// The message from Load names the actual problem — a lane pointing at a
		// model that was just deleted, a devicePool that is not a pool. Pass it
		// through rather than replacing it with something generic.
		return huma.Error400BadRequest(err.Error())
	}
	if h.Reload != nil {
		if err := h.Reload(); err != nil {
			return huma.Error500InternalServerError("saved, but the reload failed; restart to pick it up", err)
		}
	}
	return nil
}

// copyForEdit copies the maps an edit can touch, so a rejected change cannot
// leave the live config half-modified.
func copyForEdit(c *config.Config) *config.Config {
	out := *c
	out.Models = make(map[string]config.Model, len(c.Models)+1)
	for k, v := range c.Models {
		out.Models[k] = v
	}
	out.Servers = make(map[string]config.Server, len(c.Servers))
	for k, v := range c.Servers {
		out.Servers[k] = v
	}
	out.Lanes = make(map[string]config.Lane, len(c.Lanes))
	for k, v := range c.Lanes {
		out.Lanes[k] = v
	}
	return &out
}

// ModelSpec is a model as the dashboard edits it.
//
// Deliberately not config.Model: that carries a yaml.Node for `proxy` (which
// accepts a port, a host:port or an object) and a dozen fields no one edits by
// hand. This is the subset worth a form, with proxy as the string a human
// actually types.
type ModelSpec struct {
	Name          string            `json:"name" doc:"Served model name."`
	Cmd           string            `json:"cmd" required:"false" doc:"Spawn command; empty makes it a pure proxy."`
	Server        string            `json:"server" required:"false" doc:"Server it draws capacity from (required when cmd is set)."`
	Proxy         string            `json:"proxy" doc:"Where to forward: a port (5800), host:port, or a URL."`
	Upstream      string            `json:"upstream" required:"false" doc:"The id the backend knows it by, when different from the served name."`
	Type          string            `json:"type" required:"false" doc:"Cost class: chat | embed | stt | tts | …"`
	Quality       float64           `json:"quality" required:"false" doc:"Relative rank; fractional tiers are allowed."`
	MaxConcurrent int               `json:"maxConcurrent" required:"false" doc:"Admission slots."`
	MaxTokens     int               `json:"maxTokens" required:"false" doc:"max_tokens clamp when degraded onto (0 = none)."`
	Persistent    bool              `json:"persistent" required:"false" doc:"Pinned: preloaded and never evicted."`
	RAMUsage      map[string]string `json:"ramUsage" required:"false" doc:"Per-pool footprint, e.g. {\"gpu0\":\"16GB\"}."`
	Notes         string            `json:"notes" required:"false" doc:"Free text kept with the model and shown beside it."`
}

// UpsertModelInput creates or replaces one model.
type UpsertModelInput struct {
	Name string `path:"name" doc:"Served model name."`
	Body ModelSpec
}

// ConfigMutationOutput reports the result of a config edit.
type ConfigMutationOutput struct {
	Body struct {
		OK      bool   `json:"ok"`
		Message string `json:"message"`
	}
}

// UpsertModel creates or replaces a model and applies it live.
func (h *Handlers) UpsertModel(_ context.Context, in *UpsertModelInput) (*ConfigMutationOutput, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, huma.Error400BadRequest("a model needs a name")
	}
	err := h.mutateConfig(func(c *config.Config) error {
		prev, existed := c.Models[name]
		if existed && prev.Extension != "" {
			// An extension's models are derived from the extension. Editing one
			// here would be overwritten the moment the config reloads.
			return huma.Error409Conflict(fmt.Sprintf(
				"%q is provided by extension %q — edit the extension instead", name, prev.Extension))
		}
		m, err := specToModel(in.Body)
		if err != nil {
			return huma.Error400BadRequest(err.Error())
		}
		c.Models[name] = m
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := &ConfigMutationOutput{}
	out.Body.OK = true
	out.Body.Message = fmt.Sprintf("saved %s", name)
	return out, nil
}

// UpdateNotesInput sets the notes on one entity.
type UpdateNotesInput struct {
	Kind string `path:"kind" doc:"model | server | lane"`
	Name string `path:"name"`
	Body struct {
		Notes string `json:"notes"`
	}
}

// UpdateNotes edits the free text kept with an entry.
//
// Separate from the full upsert so the thing most likely to be edited — writing
// down why something is the way it is — cannot accidentally rewrite the model's
// actual configuration.
func (h *Handlers) UpdateNotes(_ context.Context, in *UpdateNotesInput) (*ConfigMutationOutput, error) {
	err := h.mutateConfig(func(c *config.Config) error {
		switch in.Kind {
		case "model":
			m, ok := c.Models[in.Name]
			if !ok {
				return huma.Error404NotFound("no such model")
			}
			m.Notes = in.Body.Notes
			c.Models[in.Name] = m
		case "server":
			s, ok := c.Servers[in.Name]
			if !ok {
				return huma.Error404NotFound("no such server")
			}
			s.Notes = in.Body.Notes
			c.Servers[in.Name] = s
		case "lane":
			l, ok := c.Lanes[in.Name]
			if !ok {
				return huma.Error404NotFound("no such lane")
			}
			l.Notes = in.Body.Notes
			c.Lanes[in.Name] = l
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
	out.Body.Message = "notes saved"
	return out, nil
}

// specToModel converts the editable shape into a config.Model, encoding proxy
// back into the yaml.Node the config schema uses.
func specToModel(s ModelSpec) (config.Model, error) {
	m := config.Model{
		Cmd: strings.TrimSpace(s.Cmd), Server: strings.TrimSpace(s.Server),
		Upstream: strings.TrimSpace(s.Upstream), Type: strings.TrimSpace(s.Type),
		Quality: s.Quality, MaxConcurrent: s.MaxConcurrent, MaxTokens: s.MaxTokens,
		Persistent: s.Persistent, RAMUsage: s.RAMUsage, Notes: s.Notes,
	}
	p := strings.TrimSpace(s.Proxy)
	if p == "" {
		return m, fmt.Errorf("proxy is required: a model must say where to forward (a port, host:port, or URL)")
	}
	var n yaml.Node
	// A bare port stays a number so it round-trips as the config's own shorthand.
	if isAllDigits(p) {
		var port int
		_, _ = fmt.Sscanf(p, "%d", &port)
		if err := n.Encode(port); err != nil {
			return m, err
		}
	} else if err := n.Encode(p); err != nil {
		return m, err
	}
	m.Proxy = n
	return m, nil
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
