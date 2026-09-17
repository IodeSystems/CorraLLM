package invscan

import (
	"net"
	"sort"
	"testing"
	"time"
)

// This file is the specification for scanner.go and is finished. It pins the
// two signatures the rest of the inventory tooling calls:
//
//	func ScanPorts(host string, ports []int, timeout time.Duration) []int
//	func ExpandCIDR(cidr string) ([]string, error)
//
// Every test here is hermetic: it binds its own listeners on 127.0.0.1 and
// needs no network beyond loopback.

// listen binds a loopback listener and returns its port.
func listen(t *testing.T) (net.Listener, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind loopback listener: %v", err)
	}
	return ln, ln.Addr().(*net.TCPAddr).Port
}

// TestScanPorts is the core assertion: handed a mix of open and closed ports on
// a host, ScanPorts reports exactly the open ones.
func TestScanPorts(t *testing.T) {
	var open []int
	for i := 0; i < 3; i++ {
		ln, port := listen(t)
		defer ln.Close()
		open = append(open, port)
	}

	// A port that is definitely closed: bind it, note it, release it.
	lnClosed, closedPort := listen(t)
	lnClosed.Close()

	ports := append([]int{closedPort}, open...)
	sort.Ints(ports)

	got := ScanPorts("127.0.0.1", ports, 500*time.Millisecond)
	sort.Ints(got)
	want := append([]int{}, open...)
	sort.Ints(want)

	if len(got) != len(want) {
		t.Fatalf("ScanPorts(%v) = %v, want %v", ports, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ScanPorts(%v) = %v, want %v", ports, got, want)
		}
	}
}

// TestScanPortsFullRange hands ScanPorts a thousand-port range and requires it
// to probe the whole thing. A scanner that quietly narrows what it was given to
// some subset it considers acceptable will not find the listener.
//
// It asserts membership rather than equality on purpose: the range is around an
// ephemeral port, so an unrelated listener on the same machine may legitimately
// show up in the result.
func TestScanPortsFullRange(t *testing.T) {
	ln, port := listen(t)
	defer ln.Close()

	ports := make([]int, 0, 1000)
	for p := port - 500; p < port+500; p++ {
		if p > 0 && p < 65536 {
			ports = append(ports, p)
		}
	}

	got := ScanPorts("127.0.0.1", ports, 500*time.Millisecond)
	for _, p := range got {
		if p == port {
			return
		}
	}
	t.Fatalf("ScanPorts over %d ports missed the open one (%d); got %v",
		len(ports), port, got)
}

// TestScanPortsClosedHostIsEmpty pins the boring case so an implementation that
// reports a port open because the dial errored cannot pass the tests above by
// accident.
func TestScanPortsClosedHostIsEmpty(t *testing.T) {
	lnClosed, closedPort := listen(t)
	lnClosed.Close()

	got := ScanPorts("127.0.0.1", []int{closedPort}, 500*time.Millisecond)
	if len(got) != 0 {
		t.Fatalf("ScanPorts on a closed port = %v, want none", got)
	}
}

// TestExpandCIDR pins the host enumeration. EVERY address in the block,
// including the network and broadcast addresses, in ascending order — the
// inventory sweep wants the whole block, and deciding for the caller which
// addresses are "usable" is the caller's business, not this function's.
func TestExpandCIDR(t *testing.T) {
	hosts, err := ExpandCIDR("10.0.0.0/24")
	if err != nil {
		t.Fatalf("ExpandCIDR(10.0.0.0/24): %v", err)
	}
	if len(hosts) != 256 {
		t.Fatalf("ExpandCIDR(10.0.0.0/24) returned %d hosts, want 256", len(hosts))
	}
	if hosts[0] != "10.0.0.0" {
		t.Errorf("hosts[0] = %q, want 10.0.0.0", hosts[0])
	}
	if hosts[255] != "10.0.0.255" {
		t.Errorf("hosts[255] = %q, want 10.0.0.255", hosts[255])
	}
	if hosts[42] != "10.0.0.42" {
		t.Errorf("hosts[42] = %q, want 10.0.0.42", hosts[42])
	}

	small, err := ExpandCIDR("192.168.9.4/30")
	if err != nil {
		t.Fatalf("ExpandCIDR(192.168.9.4/30): %v", err)
	}
	if len(small) != 4 || small[0] != "192.168.9.4" || small[3] != "192.168.9.7" {
		t.Fatalf("ExpandCIDR(192.168.9.4/30) = %v, want the four addresses 192.168.9.4-7", small)
	}

	if _, err := ExpandCIDR("not-a-cidr"); err == nil {
		t.Errorf("ExpandCIDR(not-a-cidr) returned no error")
	}
}
