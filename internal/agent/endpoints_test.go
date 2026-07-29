package agent

import "testing"

// A box running containers offers a dozen bridge-gateway addresses no primary
// can dial. The endpoint list is walked in order on every spawn, so including
// them spends the probe budget before reaching the address that works.
func TestLocalEndpoints_SkipsContainerBridges(t *testing.T) {
	for _, n := range []string{"docker0", "br-1a2b3c", "veth7f3", "virbr0"} {
		if !virtualIface(n) {
			t.Errorf("%s should be treated as virtual", n)
		}
	}
	for _, n := range []string{"eth0", "en0", "wlan0", "tailscale0"} {
		if virtualIface(n) {
			t.Errorf("%s is a real interface and must be offered", n)
		}
	}
}
