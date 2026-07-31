package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRawSpinup is the whole point of this work, asserted end to end: a real
// binary, started in a directory containing nothing, must boot, serve a UI, and
// accept the first model through the same API the dashboard uses.
//
// Every step here was a separate failure before: boot died on a missing DB
// directory, "/" 404'd with no ui/dist, and the config write was refused 409
// because no managed file existed to write into. Unit tests cover each in
// isolation; only running the binary proves they compose.
func TestRawSpinup(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the server binary")
	}

	dir := t.TempDir()
	bin := filepath.Join(dir, "corrallm")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	home := filepath.Join(dir, "home") // deliberately absent
	addr := freePort(t)

	cmd := exec.Command(bin, "serve")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"CORRALLM_HOME="+home,
		"ADDR="+addr,
		"CORRALLM_CONFIG=", // derive from home; do not inherit the developer's
		"CORRALLM_DB=",
	)
	var logs strings.Builder
	cmd.Stdout, cmd.Stderr = &logs, &logs
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		if t.Failed() {
			t.Logf("server output:\n%s", logs.String())
		}
	})

	base := "http://" + addr
	waitReady(t, base+"/health")

	t.Run("creates its home", func(t *testing.T) {
		for _, p := range []string{"admin.token", "config.yml", filepath.Join("var", "corrallm.db")} {
			if _, err := os.Stat(filepath.Join(home, p)); err != nil {
				t.Errorf("%s not created under home: %v", p, err)
			}
		}
	})

	t.Run("serves the dashboard", func(t *testing.T) {
		resp, err := http.Get(base + "/")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET / = %d, want 200 (embedded UI should serve with no ui/dist on disk)", resp.StatusCode)
		}
	})

	t.Run("reports the token path to a local caller", func(t *testing.T) {
		resp, err := http.Get(base + "/health")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var body struct {
			Status    string `json:"status"`
			TokenPath string `json:"tokenPath"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Status != "ok" {
			t.Errorf("status = %q", body.Status)
		}
		if want := filepath.Join(home, "admin.token"); body.TokenPath != want {
			t.Errorf("tokenPath = %q, want %q", body.TokenPath, want)
		}
	})

	t.Run("lists no models as an empty array", func(t *testing.T) {
		resp, err := http.Get(base + "/v1/models")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		if strings.Contains(string(b), `"data":null`) {
			t.Errorf("empty model list serialised as null, want []: %s", b)
		}
	})

	// The one that matters: a fresh instance must accept configuration through
	// the API the dashboard drives, with no file laid down by hand first.
	t.Run("accepts the first model", func(t *testing.T) {
		tok, err := os.ReadFile(filepath.Join(home, "admin.token"))
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequest(http.MethodPut,
			base+"/api/v1/config/model/first/yaml",
			strings.NewReader(`{"yaml":"proxy: https://api.example.invalid/v1\ntype: chat\n"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("PUT model = %d, want 200: %s", resp.StatusCode, b)
		}

		// It must be live, not merely written: the save reloads the config.
		resp2, err := http.Get(base + "/v1/models")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp2.Body.Close() }()
		listed, _ := io.ReadAll(resp2.Body)
		if !strings.Contains(string(listed), `"id":"first"`) {
			t.Errorf("model not served after save: %s", listed)
		}
	})
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

func waitReady(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("server never became ready at %s", url)
}
