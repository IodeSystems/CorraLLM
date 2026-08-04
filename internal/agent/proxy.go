package agent

import (
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
)

// The agent forwards DATA-PLANE traffic to the backends it spawned, so a remote
// backend needs exactly one reachable port: the agent's.
//
// Before this, the primary dialled each backend's own port directly — TargetFor
// swapped the host and kept the port — which meant every backend on an attached
// machine had to be independently reachable across the network. Two consequences,
// and the second is the serious one:
//
//   - N ports to open and keep open, all of them ephemeral and none of them
//     knowable until the model was configured.
//   - llama-server has no authentication. To be reachable from the primary it
//     had to bind a non-loopback interface, at which point anyone on that
//     network could use the model directly — no key, no quota, no accounting,
//     no admission control. corrallm's entire control surface was optional.
//
// Now backends bind loopback, and everything reaching them arrives through a
// port that requires the agent token.
//
// SSRF, considered: this forwards to an arbitrary port on 127.0.0.1, which in
// isolation would be a way to reach services the caller was never given. It adds
// no authority here, because the credential that unlocks it is the same one that
// already runs arbitrary shell commands on this machine (see the package
// comment). Anyone holding it can talk to loopback by simpler means. What the
// loopback restriction DOES buy is that a stolen token cannot turn the agent
// into a relay onto its network — that would be new authority, and it is denied.

// proxyPrefix is the route's fixed part; the path after the port is the
// backend's own, forwarded verbatim.
const proxyPrefix = "/agent/v1/proxy/"

// proxyBackend forwards a request to a local backend port.
func (s *Server) proxyBackend(w http.ResponseWriter, r *http.Request) {
	portStr := r.PathValue("port")
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		writeErr(w, http.StatusBadRequest, "proxy: bad backend port "+portStr)
		return
	}

	prefix := proxyPrefix + portStr
	target := &url.URL{Scheme: "http", Host: net.JoinHostPort("127.0.0.1", portStr)}

	rp := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host
			// Strip our own prefix so the backend sees the path the CLIENT
			// sent: /v1/chat/completions, not /agent/v1/proxy/5810/v1/....
			req.URL.Path = strings.TrimPrefix(req.URL.Path, prefix)
			if req.URL.Path == "" {
				req.URL.Path = "/"
			}
			// The agent token authenticated the hop that just ended. Forwarding
			// it would write our credential into the backend's logs for no
			// benefit — a local llama-server has no use for it.
			req.Header.Del("Authorization")
		},
		// Flush every write rather than on a timer. This carries SSE token
		// streams, where buffering shows up directly as latency the user feels:
		// tokens arriving in clumps instead of as they are generated.
		FlushInterval: -1,
	}
	rp.ServeHTTP(w, r)
}
