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
		t.Errorf("MaxQuality = %d, want 100", got)
	}
	if got := MaxQuality([]Candidate{{}, {}}); got != 0 {
		t.Errorf("MaxQuality (unset) = %d, want 0", got)
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
