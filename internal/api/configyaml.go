package api

import (
	"context"
	"fmt"
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

// ModelYAMLInput fetches one model's configuration as YAML.
type ModelYAMLInput struct {
	Name string `path:"name"`
}

// ModelYAMLOutput carries the YAML body.
type ModelYAMLOutput struct {
	Body struct {
		Name string `json:"name"`
		YAML string `json:"yaml" doc:"The model's configuration, exactly as it is stored."`
	}
}

// ModelYAML returns one model's config as YAML for editing.
func (h *Handlers) ModelYAML(_ context.Context, in *ModelYAMLInput) (*ModelYAMLOutput, error) {
	cfg := h.config()
	if cfg == nil {
		return nil, huma.Error503ServiceUnavailable("config unavailable")
	}
	m, ok := cfg.Models[in.Name]
	if !ok {
		return nil, huma.Error404NotFound(fmt.Sprintf("no model %q", in.Name))
	}
	b, err := yaml.Marshal(m)
	if err != nil {
		return nil, huma.Error500InternalServerError("could not render the model", err)
	}
	out := &ModelYAMLOutput{}
	out.Body.Name = in.Name
	out.Body.YAML = string(b)
	return out, nil
}

// PutModelYAMLInput replaces a model from YAML.
type PutModelYAMLInput struct {
	Name string `path:"name"`
	Body struct {
		YAML string `json:"yaml" doc:"The model's configuration as YAML — the same shape it has in the config file."`
	}
}

// PutModelYAML parses, validates and applies a model written as YAML.
//
// Two layers of checking, and they catch different things. Unmarshalling with
// KnownFields rejects a typo'd key — `contextPerRequst` would otherwise parse
// into nothing and silently do the opposite of what was intended. The full
// config validation then catches everything that depends on the rest of the
// config: an unknown server, a devicePool that is not a pool, a missing
// ramUsage on a host that cannot measure.
func (h *Handlers) PutModelYAML(_ context.Context, in *PutModelYAMLInput) (*ConfigMutationOutput, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, huma.Error400BadRequest("a model needs a name")
	}
	var m config.Model
	dec := yaml.NewDecoder(strings.NewReader(in.Body.YAML))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		// The decoder's message names the line and the offending key, which is
		// the whole value of showing it verbatim.
		return nil, huma.Error400BadRequest(fmt.Sprintf("invalid YAML: %v", err))
	}
	err := h.mutateConfig(func(c *config.Config) error {
		if prev, ok := c.Models[name]; ok && prev.Extension != "" {
			return huma.Error409Conflict(fmt.Sprintf(
				"%q is provided by extension %q — edit the extension instead", name, prev.Extension))
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
