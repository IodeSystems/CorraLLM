package config

import (
	"strings"
	"testing"
	"time"
)

const colmapBase = `
servers:
  box1:
    pools: { system: 125GB, gpu1: 9GB }
extensions:
  colmap:
    cmd: "exec colmap-svc"
    server: box1
    proxy: 5822
    persistent: true
    ramUsage: { system: 2GB }
    routes: [ /v1/files, /v1/uploads, /v1/reconstructions ]
    lease:
      token: ${COLMAP_LEASE_TOKEN_TEST}
      max: { gpu1: 8GB, system: 40GB }
      yieldGrace: 45s
    provides:
      sfm: { type: reconstruction }
`

func TestExtensionRoutesAndLeaseLoad(t *testing.T) {
	t.Setenv("COLMAP_LEASE_TOKEN_TEST", "s3cret")
	c, err := loadYAML(t, colmapBase)
	if err != nil {
		t.Fatal(err)
	}
	ext, prefix, ok := c.ExtensionForRoute("/v1/reconstructions/recon_1/content")
	if !ok || ext != "colmap" || prefix != "/v1/reconstructions" {
		t.Errorf("ExtensionForRoute = %q %q %v", ext, prefix, ok)
	}
	if _, _, ok := c.ExtensionForRoute("/v1/filesystem"); ok {
		t.Error("/v1/filesystem matched the /v1/files prefix")
	}
	l := c.Extensions["colmap"].Lease
	if got := l.ExpandedToken(); got != "s3cret" {
		t.Errorf("token = %q", got)
	}
	if got := l.YieldGraceDuration(); got != 45*time.Second {
		t.Errorf("yieldGrace = %s", got)
	}
	if got := l.MaxBytes()["gpu1"]; got != 8e9 {
		t.Errorf("max gpu1 = %d", got)
	}
	if (&ExtensionLease{}).YieldGraceDuration() != DefaultLeaseYieldGrace {
		t.Error("default yieldGrace")
	}
}

func TestExtensionRoutesRefused(t *testing.T) {
	for _, c := range []struct{ name, from, to, want string }{
		{"shadows builtin", "/v1/uploads", "/v1/chat/completions", "shadows"},
		{"parent of builtin", "/v1/uploads", "/v1/images", "shadows"},
		{"under builtin", "/v1/uploads", "/v1/models/x", "shadows"},
		{"not v1", "/v1/uploads", "/files", "clean /v1/"},
		{"trailing slash", "/v1/uploads", "/v1/uploads/", "clean /v1/"},
		{"wildcard", "/v1/uploads", "/v1/uploads/*", "clean /v1/"},
		{"bad grace", "yieldGrace: 45s", "yieldGrace: soon", "yieldGrace"},
		{"unknown pool", "gpu1: 8GB, system", "gpu7: 8GB, system", "gpu7"},
		{"bad size", "gpu1: 8GB", "gpu1: lots", "positive size"},
		{"no token", "token: ${COLMAP_LEASE_TOKEN_TEST}", "token: \"\"", "token"},
	} {
		_, err := loadYAML(t, strings.Replace(colmapBase, c.from, c.to, 1))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
}

// One prefix, one claimant: two extensions on /v1/files would make which one
// holds a caller's file depend on map order.
func TestExtensionRouteClaimedTwice(t *testing.T) {
	two := colmapBase + `
  other:
    cmd: "exec other"
    server: box1
    proxy: 5823
    routes: [ /v1/files/archive ]
    provides:
      x: { type: reconstruction }
`
	_, err := loadYAML(t, two)
	if err == nil || !strings.Contains(err.Error(), "already claimed") {
		t.Errorf("err = %v, want already claimed", err)
	}
}

func TestExtensionLeaseNeedsCmd(t *testing.T) {
	remote := `
extensions:
  far:
    proxy: { url: "http://10.0.0.9:5822" }
    lease: { token: t, max: { gpu1: 1GB } }
    provides:
      sfm: { type: reconstruction, upstream: sfm }
`
	_, err := loadYAML(t, remote)
	if err == nil || !strings.Contains(err.Error(), "lease needs a cmd") {
		t.Errorf("err = %v", err)
	}
}
