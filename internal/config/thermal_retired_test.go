package config

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// A stored config from before room heat was removed still has `thermal:`. It
// must load, warn once, and not survive into what is written back.
func TestRetiredThermalIgnoredWithOneWarning(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(old)
	thermalWarn = sync.Once{}

	path := filepath.Join(t.TempDir(), "c.yaml")
	src := "thermal:\n  roomF: 77\n  coolTargetF: 72\n  homeAssistant:\n    url: http://ha\npower:\n  prometheus: http://p:9090\n"
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		c, err := Load(path)
		if err != nil {
			t.Fatalf("Load with thermal: %v", err)
		}
		if c.Thermal != nil {
			t.Errorf("Thermal kept after Finalize: %v", c.Thermal)
		}
		if c.Power == nil || c.Power.Prometheus != "http://p:9090" {
			t.Errorf("power lost: %+v", c.Power)
		}
	}
	if n := strings.Count(buf.String(), "no longer used"); n != 1 {
		t.Errorf("warnings = %d, want 1:\n%s", n, buf.String())
	}
}
