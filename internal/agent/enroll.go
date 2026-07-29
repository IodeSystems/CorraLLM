package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/iodesystems/corrallm/internal/gpu"
	"github.com/iodesystems/corrallm/internal/sysmem"
)

// Enroll attaches this machine to a primary using a one-time token, returning
// the long-lived per-server credential.
//
// The agent brings its own measurements. Everything the primary needs to size
// the server — total memory, whether there is a discrete device, what addresses
// this machine answers on — is knowable HERE and tedious to type there, and a
// typed pool size is where the wrong number usually comes from.
func Enroll(ctx context.Context, primary, enrollToken, server, addr, version string) (EnrollResponse, error) {
	var out EnrollResponse

	req := EnrollRequest{
		Server:    server,
		Hello:     helloFor(version),
		Capacity:  Probe(),
		Endpoints: localEndpoints(addr),
	}
	body, err := json.Marshal(req)
	if err != nil {
		return out, err
	}
	url := strings.TrimRight(primary, "/") + "/api/v1/agents/enroll"
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return out, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Authorization", "Bearer "+enrollToken)
	hreq.Header.Set(ProtocolHeader, fmt.Sprint(Protocol))

	cli := &http.Client{Timeout: 20 * time.Second}
	resp, err := cli.Do(hreq)
	if err != nil {
		return out, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return out, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return out, err
	}
	if out.Token == "" {
		return out, fmt.Errorf("primary accepted enrollment but returned no token")
	}
	return out, nil
}

// Probe measures this machine, reporting errors rather than zeros.
func Probe() Capacity {
	var c Capacity
	const mib = 1024 * 1024
	if st, err := gpu.Probe(); err != nil {
		c.GPUError = err.Error()
	} else {
		c.GPU = &DeviceMem{Name: st.Name, TotalBytes: int64(st.TotalMiB) * mib,
			UsedBytes: int64(st.UsedMiB) * mib, FreeBytes: int64(st.FreeMiB) * mib}
	}
	if hm, err := sysmem.Probe(); err != nil {
		c.HostError = err.Error()
	} else {
		c.Host = &DeviceMem{Name: "system", TotalBytes: hm.TotalBytes,
			UsedBytes: hm.TotalBytes - hm.AvailableBytes, FreeBytes: hm.AvailableBytes}
	}
	// Ask the platform directly rather than inferring from GOOS: the answer is
	// a property of the vendor tooling present, not of the operating system.
	if _, err := gpu.ProcVRAM(); err == nil {
		c.PerProcess = true
	}
	return c
}

func helloFor(version string) Hello {
	hn, _ := os.Hostname()
	return Hello{Protocol: Protocol, Version: version, Hostname: hn,
		OS: goos(), Arch: goarch()}
}

// localEndpoints guesses the addresses this machine answers on, so the primary
// gets a candidate LIST rather than one host.
//
// Several are normal and expected: a LAN address, a VPN address, maybe more.
// Which of them the primary can actually use depends on where IT sits, and the
// agent cannot know that — so it offers all of them and lets the primary probe.
func localEndpoints(addr string) []string {
	port := "6503"
	if _, p, err := net.SplitHostPort(addr); err == nil && p != "" {
		port = p
	}
	var out []string
	ifaces, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, i := range ifaces {
		if i.Flags&net.FlagUp == 0 || virtualIface(i.Name) {
			continue
		}
		addrs, err := i.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ip, ok := a.(*net.IPNet)
			if !ok || ip.IP.IsLoopback() || ip.IP.To4() == nil {
				continue // v4, non-loopback: what a primary can realistically dial
			}
			out = append(out, "http://"+net.JoinHostPort(ip.IP.String(), port))
		}
	}
	return out
}

// virtualIface reports whether an interface is a container/VM bridge rather
// than a way anything reaches this machine.
//
// Without this a box running Docker offers a dozen 172.x gateway addresses that
// no primary can ever dial, and the endpoint list — which is walked in order on
// every spawn — spends its probe budget on them before reaching the real one.
func virtualIface(name string) bool {
	for _, p := range []string{"docker", "br-", "veth", "virbr", "vmnet", "cni", "flannel", "kube", "tailscale0.", "utun9"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// SaveToken rewrites the agent env file with the long-lived credential, so a
// restart does not try to enroll again with a token that is already spent.
func SaveToken(envPath, server, token string) error {
	if envPath == "" {
		return nil
	}
	b, _ := os.ReadFile(envPath)
	lines := strings.Split(string(b), "\n")
	set := func(key, val string) {
		for i, l := range lines {
			if strings.HasPrefix(l, key+"=") {
				lines[i] = key + "=" + val
				return
			}
		}
		lines = append(lines, key+"="+val)
	}
	set("CORRALLM_AGENT_TOKEN", token)
	set("CORRALLM_AGENT_SERVER", server)
	// The enrollment token is single-use and now spent; leaving it invites a
	// confusing "already used" failure on the next start.
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if strings.HasPrefix(l, "CORRALLM_ENROLL_TOKEN=") {
			continue
		}
		out = append(out, l)
	}
	if err := os.MkdirAll(filepath.Dir(envPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(envPath, []byte(strings.Join(out, "\n")), 0o600)
}
