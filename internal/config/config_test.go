package config

import (
	"strconv"

	"gopkg.in/yaml.v3"
	"path/filepath"
	"testing"
)

// TestMaxQuality returns the top quality (0 when none set).
func TestMaxQuality(t *testing.T) {
	if got := MaxQuality([]Candidate{{Model: Model{Quality: 60}}, {Model: Model{Quality: 100}}, {Model: Model{Quality: 40}}}); got != 100 {
		t.Errorf("MaxQuality = %v, want 100", got)
	}
	if got := MaxQuality([]Candidate{{}, {}}); got != 0 {
		t.Errorf("MaxQuality (unset) = %v, want 0", got)
	}
}

// TestResolveServed: a lane name yields its members in order (with sticky
// overrides carried); a model name pins exactly that model; unknown → miss.
func TestResolveServed(t *testing.T) {
	ttl := &Sticky{TTL: "120s"}
	c := &Config{
		Models: map[string]Model{
			"big":   {Quality: 2, Cmd: "x", Server: "s"},
			"small": {Quality: 1, Cmd: "y", Server: "s"},
		},
		Lanes: map[string]Lane{
			"chat": {Members: []LaneMember{{Model: "big"}, {Model: "small", Sticky: ttl}}},
		},
	}
	cands, ok := c.ResolveServed("chat")
	if !ok || len(cands) != 2 {
		t.Fatalf("lane resolve: ok=%v n=%d, want 2 members", ok, len(cands))
	}
	if cands[0].Name != "big" || cands[1].Name != "small" {
		t.Errorf("lane order = %s,%s; want big,small", cands[0].Name, cands[1].Name)
	}
	if cands[1].Sticky != ttl {
		t.Error("lane member sticky override not carried")
	}
	cands, ok = c.ResolveServed("small")
	if !ok || len(cands) != 1 || cands[0].Name != "small" || cands[0].Sticky != nil {
		t.Errorf("model resolve = %+v ok=%v; want pinned small, no override", cands, ok)
	}
	if _, ok := c.ResolveServed("ghost"); ok {
		t.Error("unknown served name must not resolve")
	}
}

// TestResolveServedAlias: an alias resolves to the CANONICAL name (not the
// requested one, unlike a glob), and outranks a glob template that also matches.
func TestResolveServedAlias(t *testing.T) {
	c := &Config{
		Models: map[string]Model{
			"new":    {Quality: 2, Cmd: "x", Server: "s", Aliases: []string{"legacy-id", "older-id"}},
			"qwen-*": {Quality: 1},
		},
	}
	for _, served := range []string{"legacy-id", "older-id"} {
		cands, ok := c.ResolveServed(served)
		if !ok || len(cands) != 1 {
			t.Fatalf("alias %q: ok=%v n=%d, want 1 candidate", served, ok, len(cands))
		}
		// The whole point: residency and metrics must land on one id. Carrying
		// the requested name here would split accounting for one reservation.
		if cands[0].Name != "new" {
			t.Errorf("alias %q resolved to %q, want canonical %q", served, cands[0].Name, "new")
		}
	}
	// A hand-written alias beats a wildcard that merely happens to match.
	c.Models["new"] = Model{Quality: 2, Cmd: "x", Server: "s", Aliases: []string{"qwen-legacy"}}
	cands, ok := c.ResolveServed("qwen-legacy")
	if !ok || len(cands) != 1 || cands[0].Name != "new" {
		t.Errorf("alias vs glob = %+v ok=%v; want canonical new, not the glob", cands, ok)
	}
	// An id only the glob matches still reaches the glob, keeping its own name.
	cands, ok = c.ResolveServed("qwen-other")
	if !ok || len(cands) != 1 || cands[0].Name != "qwen-other" {
		t.Errorf("glob fallthrough = %+v ok=%v; want requested name qwen-other", cands, ok)
	}
	if _, ok := c.ResolveServed("ghost"); ok {
		t.Error("unknown served name must not resolve")
	}
}

// TestValidateAliases: an alias may not be empty, globbed, self-referential, or
// shadow a model, a lane, or another model's alias — each case would load clean
// and then silently never fire.
func TestValidateAliases(t *testing.T) {
	base := func(aliases ...string) *Config {
		return &Config{
			Models: map[string]Model{
				"m":     {Proxy: portNode(1234), Aliases: aliases},
				"other": {Proxy: portNode(1235)},
			},
			Lanes: map[string]Lane{"l": {Members: []LaneMember{{Model: "m"}}}},
		}
	}
	if err := base("legacy-id").Validate(); err != nil {
		t.Fatalf("valid alias rejected: %v", err)
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("absent aliases rejected: %v", err)
	}
	for name, alias := range map[string]string{
		"empty":           "",
		"glob":            "qwen-*",
		"self":            "m",
		"shadows a model": "other",
		"shadows a lane":  "l",
	} {
		if err := base(alias).Validate(); err == nil {
			t.Errorf("alias %s (%q) must be rejected", name, alias)
		}
	}
	// Two models claiming one alias: only one could ever win, and which one
	// would depend on map order.
	c := base("dup")
	c.Models["other"] = Model{Proxy: portNode(1235), Aliases: []string{"dup"}}
	if err := c.Validate(); err == nil {
		t.Error("alias claimed by two models must be rejected")
	}
}

// TestLaneMemberScalarYAML: members accept plain string or object form.
func TestLaneMemberScalarYAML(t *testing.T) {
	var lane Lane
	if err := yamlUnmarshal(`members: ["a", {model: b, sticky: {ttl: "60s"}}]`, &lane); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(lane.Members) != 2 || lane.Members[0].Model != "a" || lane.Members[1].Model != "b" {
		t.Fatalf("members = %+v", lane.Members)
	}
	if lane.Members[1].Sticky == nil || lane.Members[1].Sticky.TTL != "60s" {
		t.Errorf("object member sticky = %+v, want ttl 60s", lane.Members[1].Sticky)
	}
}

// TestValidateLanes: member names must exist; lane names must not shadow models.
func TestValidateLanes(t *testing.T) {
	base := func() *Config {
		return &Config{
			Models: map[string]Model{"m": {Proxy: portNode(1234)}},
			Lanes:  map[string]Lane{"l": {Members: []LaneMember{{Model: "m"}}}},
		}
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("valid lane config rejected: %v", err)
	}
	c := base()
	c.Lanes["l"] = Lane{Members: []LaneMember{{Model: "ghost"}}}
	if err := c.Validate(); err == nil {
		t.Error("unknown lane member must fail validation")
	}
	c = base()
	c.Lanes["m"] = c.Lanes["l"]
	if err := c.Validate(); err == nil {
		t.Error("lane name shadowing a model must fail validation")
	}
	c = base()
	c.Lanes["l"] = Lane{}
	if err := c.Validate(); err == nil {
		t.Error("empty lane must fail validation")
	}
}

// TestValidateProxyModelRejectsResidency: residency knobs only fit cmd models.
func TestValidateProxyModelRejectsResidency(t *testing.T) {
	c := &Config{Models: map[string]Model{
		"remote": {Proxy: portNode(9999), Sticky: &Sticky{TTL: "60s"}},
	}}
	if err := c.Validate(); err == nil {
		t.Error("sticky on a proxy model must fail validation")
	}
	c = &Config{Models: map[string]Model{"nopath": {}}}
	if err := c.Validate(); err == nil {
		t.Error("model with neither cmd nor proxy must fail validation")
	}
}

// TestAcceptsQuality: a non-degrading group accepts only the top tier; a
// degrading group accepts down to its floor (P7).
func TestAcceptsQuality(t *testing.T) {
	const top = 100
	noDegrade := PriorityGroup{}
	if !noDegrade.AcceptsQuality(100, top) {
		t.Error("non-degrade group must accept the top tier")
	}
	if noDegrade.AcceptsQuality(60, top) {
		t.Error("non-degrade group must reject below the top tier")
	}

	floor60 := PriorityGroup{AcceptDegrade: true, QualityFloor: 60}
	if !floor60.AcceptsQuality(60, top) {
		t.Error("degrade group must accept at its floor")
	}
	if floor60.AcceptsQuality(40, top) {
		t.Error("degrade group must reject below its floor")
	}

	anyQ := PriorityGroup{AcceptDegrade: true}
	if !anyQ.AcceptsQuality(0, top) {
		t.Error("degrade group with no floor must accept anything")
	}

	// Regression: a group with a floor must still accept a model whose whole
	// ladder is below that floor (audio backends default to quality 0). The
	// model's top tier is always acceptable — the floor only gates degrading
	// below the best when a better tier exists.
	floored := PriorityGroup{AcceptDegrade: true, QualityFloor: 1}
	if !floored.AcceptsQuality(0, 0) {
		t.Error("floor must not reject a single-tier quality-0 model (audio)")
	}
	if !floored.AcceptsQuality(1, 1) {
		t.Error("floor must accept a model whose top tier equals the floor")
	}
}

// TestLoadSampleConfig parses the committed corrallm.yaml and checks the shape
// the scheduler will rely on — and that Validate accepts it.
func TestLoadSampleConfig(t *testing.T) {
	c, err := Load(filepath.Join("..", "..", "corrallm.yaml"))
	if err != nil {
		t.Fatalf("load sample config: %v", err)
	}
	if _, ok := c.Servers["box1"]; !ok {
		t.Errorf("expected server box1, got %v", keysOf(c.Servers))
	}
	m, ok := c.Models["qwen3-coder"]
	if !ok {
		t.Fatalf("expected model qwen3-coder, got %v", keysOf(c.Models))
	}
	if m.Cmd == "" {
		t.Error("qwen3-coder: expected a cmd model")
	}
	lane, ok := c.Lanes["chat"]
	if !ok || len(lane.Members) < 2 {
		t.Errorf("expected lane chat with ≥2 members, got %+v", lane)
	}
	if c.Keys["aw3"] != "interactive" {
		t.Errorf("key aw3: want interactive, got %q", c.Keys["aw3"])
	}
}

// TestLoadMissingIsEmpty: a missing config file yields an empty, valid config.
func TestLoadMissingIsEmpty(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatalf("missing config should not error: %v", err)
	}
	if len(c.Models) != 0 {
		t.Errorf("missing config should be empty, got %d models", len(c.Models))
	}
}

// TestValidateRejectsUnknownServer: a model referencing an undeclared server
// must fail validation.
func TestValidateRejectsUnknownServer(t *testing.T) {
	c := &Config{
		Models: map[string]Model{
			"m": {Cmd: "x", Server: "ghost"},
		},
	}
	if err := c.Validate(); err == nil {
		t.Fatal("expected error for unknown server, got nil")
	}
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// yamlUnmarshal is a tiny test shim over yaml.v3.
func yamlUnmarshal(s string, out any) error { return yaml.Unmarshal([]byte(s), out) }

// portNode builds the scalar `proxy: <port>` node form.
func portNode(port int) yaml.Node {
	var n yaml.Node
	n.SetString(strconv.Itoa(port))
	return n
}

// TestValidateSamplingDefault: an unrecognised default has NO error surface at
// request time — it just reads as "instruct" — so every unmarked request would
// quietly get the wrong mode's sampler. It has to fail at load or not at all.
func TestValidateSamplingDefault(t *testing.T) {
	temp := 0.7
	base := func(def string) *Config {
		return &Config{Models: map[string]Model{"m": {
			Proxy:    portNode(1234),
			Sampling: &SamplingConfig{Instruct: SamplingProfile{Temperature: &temp}, Default: def},
		}}}
	}
	for _, ok := range []string{"", "instruct", "thinking"} {
		if err := base(ok).Validate(); err != nil {
			t.Errorf("default %q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"thinkng", "Thinking", "on", "auto"} {
		if err := base(bad).Validate(); err == nil {
			t.Errorf("default %q must be rejected", bad)
		}
	}
	// A sampling block with neither profile does nothing at all; saying so at
	// load beats an operator wondering why their config has no effect.
	c := &Config{Models: map[string]Model{"m": {Proxy: portNode(1234), Sampling: &SamplingConfig{}}}}
	if err := c.Validate(); err == nil {
		t.Error("an empty sampling block must be rejected")
	}
}
