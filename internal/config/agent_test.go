package config

import (
	"strings"
	"testing"
)

const agentBase = `
servers:
  box1:
    pools: { gpu0: 30GB }
  mac1:
    pools: { system: 64GB }
    devicePool: system
    agent:
      endpoints:
        - http://192.168.1.42:6503
        - http://100.64.0.3:6503
        - https://mac.example.com:6503
models:
  local-mtp:
    cmd: "exec llama-server"
    server: box1
    proxy: 5800
  mac-qwen:
    cmd: "exec rapid-mlx serve"
    server: mac1
    proxy: 5810
`

// An agent has SEVERAL addresses at once — LAN, VPN, external — and which one
// works depends on where the daemon sits. All of them must survive parsing, in
// order, because order is preference.
func TestAgent_EndpointsAreAList(t *testing.T) {
	c, err := loadYAML(t, agentBase)
	if err != nil {
		t.Fatal(err)
	}
	a := c.Servers["mac1"].Agent
	if a == nil {
		t.Fatal("agent binding did not parse")
	}
	if len(a.Endpoints) != 3 {
		t.Fatalf("endpoints = %v, want 3 preserved in order", a.Endpoints)
	}
	if got := a.Host(); got != "192.168.1.42" {
		t.Errorf("Host() = %q, want the first endpoint's host", got)
	}
}

// THE hazard. A model on an agent-bound server is written the normal way —
// `proxy: 5810`, "the port my backend listens on" — which resolves to
// 127.0.0.1:5810: the PRIMARY's loopback. Left alone, the daemon would forward
// a Mac model's traffic to whatever local process happens to hold that port and
// return its answers.
func TestAgent_TargetForDoesNotResolveToThePrimarysLoopback(t *testing.T) {
	c, err := loadYAML(t, agentBase)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := c.Models["mac-qwen"].ProxyTarget()
	if err != nil {
		t.Fatal(err)
	}
	if !IsLocalHost(raw.URL.Hostname()) {
		t.Fatalf("precondition: bare port should resolve to loopback, got %s", raw.URL)
	}

	got, err := c.TargetFor("mac-qwen", c.Models["mac-qwen"])
	if err != nil {
		t.Fatal(err)
	}
	if want := "http://192.168.1.42:5810"; got.URL.String() != want {
		t.Errorf("TargetFor = %s, want %s (agent host, model's own port)", got.URL, want)
	}

	// A model on a normal server is untouched: this must not change any
	// existing single-box config.
	local, err := c.TargetFor("local-mtp", c.Models["local-mtp"])
	if err != nil {
		t.Fatal(err)
	}
	if want := "http://127.0.0.1:5800"; local.URL.String() != want {
		t.Errorf("TargetFor(local) = %s, want %s unchanged", local.URL, want)
	}
}

// An explicitly written host is the operator's statement of fact and must win;
// TargetFor only fills in a host that was never stated.
func TestAgent_ExplicitHostWins(t *testing.T) {
	c, err := loadYAML(t, `
servers:
  mac1:
    pools: { system: 64GB }
    devicePool: system
    agent: { endpoints: ["http://192.168.1.42:6503"] }
models:
  pinned:
    cmd: "exec x"
    server: mac1
    proxy: { host: 10.9.9.9, port: 5810 }
`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.TargetFor("pinned", c.Models["pinned"])
	if err != nil {
		t.Fatal(err)
	}
	if got.URL.Hostname() != "10.9.9.9" {
		t.Errorf("host = %s, want the explicitly written 10.9.9.9", got.URL.Hostname())
	}
}

// A binding that cannot be dialled must fail at LOAD. Otherwise every model on
// that server is admitted and then fails at spawn, one request at a time.
func TestAgent_BadBindingFailsAtLoad(t *testing.T) {
	for name, body := range map[string]string{
		"no endpoints": `
servers:
  mac1: { pools: { system: 1GB }, agent: {} }
`,
		"not a url": `
servers:
  mac1: { pools: { system: 1GB }, agent: { endpoints: ["mac.lan:6503"] } }
`,
		"no host": `
servers:
  mac1: { pools: { system: 1GB }, agent: { endpoints: ["http://"] } }
`,
	} {
		if _, err := loadYAML(t, body); err == nil {
			t.Errorf("%s: want a load error", name)
		} else if !strings.Contains(err.Error(), "agent") {
			t.Errorf("%s: err = %v, want it to name the agent binding", name, err)
		}
	}
}

// No agent binding = every existing config. Nothing may change.
func TestAgent_AbsentBindingChangesNothing(t *testing.T) {
	c, err := loadYAML(t, `
servers:
  box1: { pools: { gpu0: 30GB } }
models:
  m: { cmd: "exec x", server: box1, proxy: 5800 }
`)
	if err != nil {
		t.Fatal(err)
	}
	if c.Servers["box1"].Agent != nil {
		t.Fatal("agent should be nil when undeclared")
	}
	got, err := c.TargetFor("m", c.Models["m"])
	if err != nil {
		t.Fatal(err)
	}
	if got.URL.String() != "http://127.0.0.1:5800" {
		t.Errorf("TargetFor = %s, want the unchanged loopback target", got.URL)
	}
}

// A host that cannot attribute memory per process has no way to ever measure a
// model, so "reserve the whole pool, then measure" never measures — the server
// silently serves one model at a time, forever, with no error anywhere. Require
// the only number anyone can have instead.
func TestValidate_UnmeasurableHostRequiresRAMUsage(t *testing.T) {
	body := `
servers:
  mac1:
    pools: { system: 64GB }
    devicePool: system
    noProcessMemory: true
    agent: { endpoints: ["http://mac.lan:6503"] }
models:
  m:
    cmd: "exec rapid-mlx serve"
    server: mac1
    proxy: 5810
`
	if _, err := loadYAML(t, body); err == nil {
		t.Fatal("want a load error for a model with no ramUsage on an unmeasurable host")
	} else if !strings.Contains(err.Error(), "ramUsage is required") {
		t.Errorf("err = %v, want it to name the missing ramUsage", err)
	}

	// With ramUsage it loads: the operator has supplied the size that cannot be
	// measured.
	withUsage := strings.Replace(body, "    proxy: 5810", "    proxy: 5810\n    ramUsage: { system: 20GB }", 1)
	if _, err := loadYAML(t, withUsage); err != nil {
		t.Errorf("declaring ramUsage should satisfy the rule: %v", err)
	}
}

// A MEASURABLE host keeps the old behaviour: ramUsage stays advisory, because
// the tune profile will supersede it.
func TestValidate_MeasurableHostDoesNotRequireRAMUsage(t *testing.T) {
	if _, err := loadYAML(t, `
servers:
  box1: { pools: { gpu0: 30GB } }
models:
  m: { cmd: "exec llama-server", server: box1, proxy: 5800 }
`); err != nil {
		t.Errorf("a measurable host must not require ramUsage: %v", err)
	}
}
