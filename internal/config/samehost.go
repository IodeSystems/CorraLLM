package config

import (
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

// SameMachine reports whether host names THIS machine: loopback, this
// machine's hostname, or one of its own interface addresses.
//
// It decides whether a model on an agent-bound server is dialled through the
// agent's proxy or straight at its backend's loopback port (see TargetFor).
// An agent on the same box as the primary is reached at its loopback or LAN
// address, and both must count — enrollment records whichever addresses the
// agent reported, which on a LAN box is usually the LAN one.
//
// Errs toward "not this machine": a hostname that is not ours, an address
// that cannot be parsed, or interfaces that cannot be listed all keep the
// agent's proxy path, which works from anywhere. The opposite mistake would
// send traffic to a port on the wrong machine.
func SameMachine(host string) bool {
	h := strings.ToLower(strings.Trim(strings.TrimSpace(host), "[]"))
	if IsLocalHost(h) {
		return true
	}
	if hn, err := os.Hostname(); err == nil && h == strings.ToLower(hn) {
		return true
	}
	ip := net.ParseIP(h)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	for _, own := range ownAddrs() {
		if own.Equal(ip) {
			return true
		}
	}
	return false
}

// interfaceAddrs is swappable so tests can pretend to own an address.
var interfaceAddrs = net.InterfaceAddrs

// ownAddrs caches this machine's interface addresses briefly. TargetFor runs on
// every load, and a laptop's addresses do change (VPN up, new DHCP lease), so
// the cache is short rather than forever.
var ownAddrCache struct {
	sync.Mutex
	at    time.Time
	addrs []net.IP
}

const ownAddrTTL = 30 * time.Second

func ownAddrs() []net.IP {
	ownAddrCache.Lock()
	defer ownAddrCache.Unlock()
	if ownAddrCache.addrs != nil && time.Since(ownAddrCache.at) < ownAddrTTL {
		return ownAddrCache.addrs
	}
	as, err := interfaceAddrs()
	if err != nil {
		return nil
	}
	out := make([]net.IP, 0, len(as))
	for _, a := range as {
		switch v := a.(type) {
		case *net.IPNet:
			out = append(out, v.IP)
		case *net.IPAddr:
			out = append(out, v.IP)
		}
	}
	ownAddrCache.addrs, ownAddrCache.at = out, time.Now()
	return out
}

// resetOwnAddrs drops the cache (tests).
func resetOwnAddrs() {
	ownAddrCache.Lock()
	ownAddrCache.addrs = nil
	ownAddrCache.Unlock()
}
