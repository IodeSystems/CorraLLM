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
	// Traffic goes to the AGENT's port and names the backend's port in the path,
	// rather than dialling the backend directly — see TargetFor for why that
	// stopped being acceptable (an unauthenticated backend on a reachable
	// interface). The durable invariant is unchanged: never the primary.
	if want := "http://192.168.1.42:6503"; got.URL.String() != want {
		t.Errorf("TargetFor URL = %s, want the agent's own address %s", got.URL, want)
	}
	if want := "/agent/v1/proxy/5810"; got.BasePath != want {
		t.Errorf("BasePath = %q, want %q (the backend's port, forwarded by the agent)", got.BasePath, want)
	}
	// The agent gates its data plane with the same token as its control plane,
	// so the request has to carry it or every completion 401s.
	if got.Headers["Authorization"] == "" {
		t.Error("no Authorization header: the agent would reject every request")
	}
	// The composed prefix is what callers actually hang a path off.
	if want := "http://192.168.1.42:6503/agent/v1/proxy/5810"; got.BaseURLString() != want {
		t.Errorf("BaseURLString = %q, want %q", got.BaseURLString(), want)
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
// ramUsage is NOT required, on any host.
//
// It used to be mandatory wherever per-process memory could not be measured,
// because the fallback ("reserve the whole pool, then measure") never reached
// the measuring part there and the server silently became one-model-at-a-time.
// That was a workaround for a missing implementation: unified-memory hosts can
// attribute memory to a process group — the resident set IS the footprint — so
// measurement governs there like everywhere else.
//
// It matters that this is not merely relaxed but INVERTED: every declaration
// observed in production was wrong (16GB declared against 33.7GB measured), and
// a wrong declaration is worse than an absent one, because absence is honest
// and takes the conservative path.
func TestValidate_RAMUsageIsNeverRequired(t *testing.T) {
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
	if _, err := loadYAML(t, body); err != nil {
		t.Fatalf("a model with no ramUsage must load and be measured, not refused: %v", err)
	}

	// Declaring one is still allowed — it is a bootstrap hint that saves the
	// first heavy-handed eviction, and measurement supersedes it either way.
	withUsage := strings.Replace(body, "    proxy: 5810", "    proxy: 5810\n    ramUsage: { system: 20GB }", 1)
	if _, err := loadYAML(t, withUsage); err != nil {
		t.Errorf("declaring ramUsage must still be accepted: %v", err)
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
