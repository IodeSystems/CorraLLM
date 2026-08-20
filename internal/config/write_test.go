package config

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// A config that came from Load is NOT the same shape as one that goes into it:
// resolveExtensions expands every extension's `provides` into Models. Writing
// that back out emits the extension AND its expansion, and the next Load fails
// with "collides with a declared model" — a file the daemon cannot start from.
func TestForWriting_DoesNotEmitDerivedExtensionModels(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "in.yaml")
	if err := os.WriteFile(src, []byte(`
servers:
  box1: { pools: { system: 8GB } }
extensions:
  oidio:
    cmd: "exec oidio"
    server: box1
    proxy: 5806
    ramUsage: { system: 1GB }
    provides:
      stt: { type: stt }
      tts: { type: tts }
`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(src)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Models["oidio-stt"]; !ok {
		t.Fatal("precondition: Load should have expanded the extension")
	}

	// ForWriting is what every persistence path applies before storing. Round
	// tripping through YAML here stands in for whichever store is on the other
	// side: the assertion is that what it produces LOADS AGAIN, because a config
	// that does not is a daemon that cannot restart.
	b, err := yaml.Marshal(ForWriting(c))
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "authored.yml")
	if err := os.WriteFile(out, b, 0o600); err != nil {
		t.Fatal(err)
	}
	back, err := Load(out)
	if err != nil {
		t.Fatalf("the authored config does not load — the daemon could not restart: %v", err)
	}
	if _, ok := back.Models["oidio-stt"]; !ok {
		t.Error("the extension's models did not come back on reload")
	}
}
