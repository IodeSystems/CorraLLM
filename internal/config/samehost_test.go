package config

import (
	"fmt"
	"net"
	"testing"
)

func agentCfg(t *testing.T, endpoint string) *Config {
	t.Helper()
	c, err := LoadBytesForTest([]byte(fmt.Sprintf(`
servers:
  box1:
    pools: { gpu0: 30GB }
    agent: { endpoints: [%q], token: t }
models:
  m: { cmd: "exec llama-server", server: box1, proxy: 5810 }
`, endpoint)))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// An agent on THIS machine is bypassed for data: requests go straight to the
// backend's loopback port, so restarting the agent drops none of them. An agent
// elsewhere keeps the proxy path — dialling its backend directly would need an
// unauthenticated port open on the network.
func TestTargetFor_SameMachineAgentDialsTheBackendDirectly(t *testing.T) {
	oldAddrs := interfaceAddrs
	interfaceAddrs = func() ([]net.Addr, error) {
		return []net.Addr{&net.IPNet{IP: net.ParseIP("10.9.8.7"), Mask: net.CIDRMask(24, 32)}}, nil
	}
	resetOwnAddrs()
	t.Cleanup(func() { interfaceAddrs = oldAddrs; resetOwnAddrs() })

	cases := []struct {
		endpoint string
		direct   bool
	}{
		{"http://127.0.0.1:6503", true},
		{"http://localhost:6503", true},
		{"http://10.9.8.7:6503", true}, // one of our own interface addresses
		{"http://192.168.1.42:6503", false},
	}
	for _, c := range cases {
		cfg := agentCfg(t, c.endpoint)
		got, err := cfg.TargetFor("m", cfg.Models["m"])
		if err != nil {
			t.Fatal(err)
		}
		if c.direct {
			if got.URL.String() != "http://127.0.0.1:5810" || got.BasePath != "" {
				t.Errorf("%s: target %s%s, want the backend's loopback port directly", c.endpoint, got.URL, got.BasePath)
			}
		} else if got.BasePath != "/agent/v1/proxy/5810" {
			t.Errorf("%s: target %s%s, want the agent proxy path", c.endpoint, got.URL, got.BasePath)
		}
	}
}
