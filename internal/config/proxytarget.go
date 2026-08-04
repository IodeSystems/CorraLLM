package config

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// ProxyTarget is a backend's resolved forward destination: a base URL plus any
// auth headers to inject (for remote/paid endpoints).
type ProxyTarget struct {
	URL     *url.URL
	Headers map[string]string

	// BasePath is a path PREFIX prepended to the incoming request path when the
	// upstream mounts its OpenAI surface below root. The client always sends the
	// standard /v1/... path; a remote like Groq serves it at
	// /openai/v1/chat/completions, so its BasePath is "/openai" (NOT "/openai/v1"
	// — the /v1 already arrives on the request). OpenRouter is "/api";
	// llama.cpp and most others mount at root and leave this "". Empty is a
	// no-op, so local backends are unaffected.
	BasePath string

	// Model is the upstream's own model id, substituted into the request body's
	// `model` field on the way out. corrallm routes on the SERVED name
	// (e.g. "groq-llama-70b"), but the remote expects its own id
	// (e.g. "llama-3.3-70b-versatile"); a local llama.cpp backend ignores the
	// field entirely, so this is empty for them and the body forwards unchanged.
	Model string

	// AuthTokenCommand, when set, is a command run per-request (result cached
	// briefly) whose stdout is injected as `Authorization: Bearer <out>`. Used
	// for short-lived rotating credentials — e.g. reusing Claude Code's OAuth
	// subscription token, whose own credential store keeps it refreshed:
	//   authTokenCommand: jq -r .claudeAiOauth.accessToken ~/.claude/.credentials.json
	// A static `Authorization` in `headers` still wins if both are set.
	AuthTokenCommand string
}

// BaseURLString is the full prefix a caller should hang a path off: the target
// URL plus its base path.
//
// It exists because those two were routinely combined by hand, and the ones that
// forgot were only correct while BasePath was empty. Routing agent-hosted
// backends through the agent gave every one of them a base path, which turned
// "/health" into a probe of the AGENT's root instead of the backend's.
func (t *ProxyTarget) BaseURLString() string {
	if t == nil || t.URL == nil {
		return ""
	}
	return strings.TrimRight(t.URL.String(), "/") + t.BasePath
}

// proxyObj is the object form of `proxy:`
// ({host, port, headers, basePath, model}).
type proxyObj struct {
	Host             string            `yaml:"host"`
	Port             int               `yaml:"port"`
	Scheme           string            `yaml:"scheme"`
	Headers          map[string]string `yaml:"headers"`
	BasePath         string            `yaml:"basePath"`
	Model            string            `yaml:"model"`
	AuthTokenCommand string            `yaml:"authTokenCommand"`
}

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// ProxyTarget resolves the backend's `proxy:` field, which may be:
//
//	8081                                    → http://127.0.0.1:8081
//	"host:port" / "http://host:port/…"      → as written (http:// default)
//	{ host, port, scheme?, headers? }       → built from parts
//
// ${ENV} references in header values are expanded from the process env (so
// secrets stay out of the committed YAML). A spawned backend (cmd set) that
// declares just a port proxies to localhost.
func (m Model) ProxyTarget() (*ProxyTarget, error) {
	t, err := m.proxyTarget()
	if err != nil {
		return nil, err
	}
	// A per-model upstream id wins over the proxy object's, so an extension's
	// shared target can still serve models the backend knows by different names.
	if m.Upstream != "" {
		t.Model = m.Upstream
	}
	return t, nil
}

func (m Model) proxyTarget() (*ProxyTarget, error) {
	n := m.Proxy
	switch n.Kind {
	case 0:
		return nil, fmt.Errorf("model has no proxy target")
	case yaml.ScalarNode:
		s := n.Value
		if port, err := strconv.Atoi(s); err == nil {
			return targetFromHostPort("127.0.0.1", port, "http", nil)
		}
		return targetFromString(s)
	case yaml.MappingNode:
		var o proxyObj
		if err := n.Decode(&o); err != nil {
			return nil, fmt.Errorf("proxy object: %w", err)
		}
		host := o.Host
		if host == "" {
			host = "127.0.0.1"
		}
		scheme := o.Scheme
		if scheme == "" {
			if o.Port == 443 {
				scheme = "https"
			} else {
				scheme = "http"
			}
		}
		t, err := targetFromHostPort(host, o.Port, scheme, expandHeaders(o.Headers))
		if err != nil {
			return nil, err
		}
		t.BasePath = normalizeBasePath(o.BasePath)
		t.Model = o.Model
		t.AuthTokenCommand = o.AuthTokenCommand
		return t, nil
	default:
		return nil, fmt.Errorf("unsupported proxy target kind %d", n.Kind)
	}
}

func targetFromHostPort(host string, port int, scheme string, headers map[string]string) (*ProxyTarget, error) {
	hostport := host
	if port != 0 {
		hostport = fmt.Sprintf("%s:%d", host, port)
	}
	u, err := url.Parse(fmt.Sprintf("%s://%s", scheme, hostport))
	if err != nil {
		return nil, err
	}
	return &ProxyTarget{URL: u, Headers: headers}, nil
}

func targetFromString(s string) (*ProxyTarget, error) {
	if u, err := url.Parse(s); err == nil && u.Scheme != "" && u.Host != "" {
		return &ProxyTarget{URL: u}, nil
	}
	// Bare "host:port".
	u, err := url.Parse("http://" + s)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("invalid proxy target %q", s)
	}
	return &ProxyTarget{URL: u}, nil
}

// normalizeBasePath cleans a configured base-path prefix: "" stays "" (a
// no-op), and a non-empty value is forced to a single leading slash and no
// trailing slash so the Director's join is unambiguous ("openai/" → "/openai").
func normalizeBasePath(p string) string {
	p = strings.Trim(p, "/")
	if p == "" {
		return ""
	}
	return "/" + p
}

func expandHeaders(h map[string]string) map[string]string {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string]string, len(h))
	for k, v := range h {
		out[k] = envRef.ReplaceAllStringFunc(v, func(m string) string {
			name := envRef.FindStringSubmatch(m)[1]
			return os.Getenv(name)
		})
	}
	return out
}

// LocalProcess reports whether a process on THIS box backs the model — its own
// cmd, or the cmd of the extension hosting it. A provided model carries no cmd
// of its own (that lives on the extension), so the ExtensionHosted flag is what
// separates oidio-stt (a real local process, shared with its siblings) from
// groq-llama-70b (nothing to spawn).
func (m Model) LocalProcess() bool {
	return strings.TrimSpace(m.Cmd) != "" || (m.Extension != "" && m.ExtensionHosted)
}

// Remote reports whether the model is served by a host corrallm does not run:
// no local process, and a forward target that is not this box.
//
// This is the predicate for "holds no residency". A remote model is never
// loaded, never evicted and consumes no pool — so reporting a residency state
// for it (which used to read "ready" from the first request onward, forever)
// described nothing real. Note the loopback test: a pure-proxy model can point
// at a LOCAL port some other model spawns, and that one is not remote.
func (m Model) Remote() bool {
	if m.LocalProcess() {
		return false
	}
	t, err := m.ProxyTarget()
	if err != nil || t == nil || t.URL == nil {
		return false
	}
	return !IsLocalHost(t.URL.Hostname())
}

// IsLocalHost reports whether a proxy target's host is this machine — loopback
// or empty. A private-LAN address is deliberately NOT local: another box on the
// LAN is a host we do not manage, which is exactly what Remote means.
func IsLocalHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	return h == "" || h == "localhost" || h == "::1" || strings.HasPrefix(h, "127.")
}

// Provider is a coarse vendor label for cost/usage metrics: "local" for a
// spawned backend, else the upstream inferred from the proxy target host
// (anthropic|openrouter|openai|groq|google|…), falling back to the bare host.
func (m Model) Provider() string {
	if strings.TrimSpace(m.Cmd) != "" {
		return "local"
	}
	t, err := m.ProxyTarget()
	if err != nil || t == nil || t.URL == nil {
		return "unknown"
	}
	h := strings.ToLower(t.URL.Hostname())
	switch {
	case strings.Contains(h, "anthropic"):
		return "anthropic"
	case strings.Contains(h, "openrouter"):
		return "openrouter"
	case strings.Contains(h, "openai"):
		return "openai"
	case strings.Contains(h, "groq"):
		return "groq"
	case strings.Contains(h, "google"), strings.Contains(h, "gemini"):
		return "google"
	case h == "", h == "localhost", strings.HasPrefix(h, "127."), strings.HasPrefix(h, "192.168."), strings.HasPrefix(h, "10."):
		return "local"
	default:
		return h
	}
}

// TargetFor resolves a served model's forward destination, accounting for the
// machine its server actually runs on.
//
// The hazard this removes: a model on an agent-bound server written the normal
// way — `proxy: 5810`, meaning "the port my backend listens on" — resolves
// through proxyTarget to http://127.0.0.1:5810, which is the PRIMARY's
// loopback. On a box where something else happens to hold that port, the
// daemon would forward a Mac model's traffic to an unrelated local process and
// return its answers. A confusing 502 would be the good outcome.
//
// So for a model whose server has an agent, a loopback host is rewritten to the
// agent's preferred endpoint host, keeping the model's own port. A host the
// operator wrote explicitly always wins — this only fills in the host that was
// never stated.
//
// Endpoint choice is first-listed for now; see AgentBinding.Endpoints.
func (c *Config) TargetFor(served string, m Model) (*ProxyTarget, error) {
	t, err := m.ProxyTarget()
	if err != nil || t == nil || t.URL == nil {
		return t, err
	}
	srv, ok := c.Servers[m.Server]
	if !ok || srv.Agent == nil {
		return t, nil
	}
	if !IsLocalHost(t.URL.Hostname()) {
		return t, nil // explicitly addressed elsewhere; leave it alone
	}
	agentBase := srv.Agent.BaseURL()
	if agentBase == nil {
		return t, nil
	}
	backendPort := t.URL.Port()
	if backendPort == "" {
		// Nothing to forward to: without a port there is no local service to
		// name. Leave the target alone rather than inventing one.
		return t, nil
	}

	// Route through the AGENT rather than at the backend directly.
	//
	// This used to swap the host and keep the backend's port, so the primary
	// dialled <mac>:5810 itself. That required every backend port to be
	// reachable across the network — and llama-server has no authentication, so
	// reaching it meant binding a non-loopback interface, at which point anyone
	// on that network could use the model with no key, no quota, no accounting
	// and no admission control. The control plane was effectively optional.
	//
	// Now the backend's port becomes a path segment on the agent's own port. One
	// reachable port per machine instead of N, every request carries the agent
	// token, and backends can bind loopback where nothing else can reach them.
	u := *agentBase
	out := *t
	out.URL = &u
	// BasePath rather than a path on the URL: the proxy prepends it to the
	// CLIENT's path, so /v1/chat/completions arrives at the backend unchanged
	// after the agent strips the prefix.
	out.BasePath = joinBasePath(t.BasePath, "/agent/v1/proxy/"+backendPort)
	// Copy before writing: Model values are shared, and mutating the map would
	// reach whatever else holds this target.
	hdr := make(map[string]string, len(t.Headers)+1)
	for k, v := range t.Headers {
		hdr[k] = v
	}
	// The agent gates its data plane with the same token as its control plane.
	hdr["Authorization"] = "Bearer " + srv.Agent.ExpandedToken()
	out.Headers = hdr
	return &out, nil
}

// joinBasePath puts the agent's proxy prefix in FRONT of any base path the model
// already had.
//
// Order matters and is easy to get backwards: the agent has to see its own
// prefix first to know which backend to forward to, and the model's own prefix
// is part of the path the BACKEND expects. Reversed, the agent would not
// recognise the route at all.
func joinBasePath(modelBase, agentPrefix string) string {
	if modelBase == "" {
		return agentPrefix
	}
	return agentPrefix + modelBase
}

// ExpandedToken resolves ${ENV} references in the agent token, the same way
// proxy headers are expanded — so the secret lives in the environment and never
// in tracked YAML.
func (a *AgentBinding) ExpandedToken() string {
	if a == nil {
		return ""
	}
	return envRef.ReplaceAllStringFunc(a.Token, func(m string) string {
		return os.Getenv(envRef.FindStringSubmatch(m)[1])
	})
}
