package host

import (
	"net"
	"strings"
	"testing"
)

// TestStartRefusesTakenPort: a copy of the backend left running in a terminal
// must fail the spawn with the reason, not race the health check.
func TestStartRefusesTakenPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	_, err = NewLocal("box").Start(Spec{Name: "m", Cmd: "true", Port: port, Out: &strings.Builder{}})
	if err == nil || !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("Start on a taken port: err = %v", err)
	}
	if err := portFree(0); err != nil {
		t.Fatalf("port 0 (unknown) must not be checked: %v", err)
	}
}
