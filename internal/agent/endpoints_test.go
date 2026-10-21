package agent

import "testing"

// An agent bound to one address is reachable there and nowhere else. Offering
// every interface sent the primary to addresses nothing listens on: an agent on
// 127.0.0.1 beside its primary lost its only working endpoint on the first beat.
func TestLocalEndpoints_BoundAddressIsTheOnlyEndpoint(t *testing.T) {
	got := localEndpoints("127.0.0.1:6612")
	if len(got) != 1 || got[0] != "http://127.0.0.1:6612" {
		t.Errorf("localEndpoints(127.0.0.1:6612) = %v, want only the bound address", got)
	}
	for _, e := range localEndpoints("0.0.0.0:6612") {
		if e == "http://0.0.0.0:6612" {
			t.Error("the unspecified address was offered as an endpoint")
		}
	}
}

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
