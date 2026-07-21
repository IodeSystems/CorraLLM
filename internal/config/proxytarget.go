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
}

// proxyObj is the object form of `proxy:`
// ({host, port, headers, basePath, model}).
type proxyObj struct {
	Host     string            `yaml:"host"`
	Port     int               `yaml:"port"`
	Scheme   string            `yaml:"scheme"`
	Headers  map[string]string `yaml:"headers"`
	BasePath string            `yaml:"basePath"`
	Model    string            `yaml:"model"`
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
