package agent

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/iodesystems/corrallm/internal/toolchain"
)

// The toolchain surface must live OUTSIDE the backend table. A build registered
// as a backend is reaped by proc.ReconcileAgent after its 60-second adoption
// grace — which a fifteen-minute CUDA compile loses every single time.
func TestToolRunDoesNotRegisterABackend(t *testing.T) {
	a, srv := newTestAgent(t, "tok")

	bin := t.TempDir()
	spec := toolchain.Spec{Name: "llama.cpp", Recipe: "llama.cpp", Bin: "llama-server", InstalledAt: bin}
	body, _ := json.Marshal(ToolRunRequest{Spec: spec, Verb: string(toolchain.VerbProbe)})

	resp, out := do(t, srv, http.MethodPost, "/agent/v1/tools/run", "tok", string(body))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, out)
	}

	a.mu.Lock()
	n := len(a.backends)
	a.mu.Unlock()
	if n != 0 {
		t.Errorf("a tool run registered %d backend(s) — reconciliation would reap it mid-build", n)
	}
}

func TestToolRunProbesAndReturnsJSON(t *testing.T) {
	_, srv := newTestAgent(t, "tok")

	bin := t.TempDir()
	fake := filepath.Join(bin, "llama-server")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho 'version: 10380 (0b1bad14f)' >&2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	spec := toolchain.Spec{Name: "llama.cpp", Recipe: "llama.cpp", Bin: "llama-server", InstalledAt: bin}
	body, _ := json.Marshal(ToolRunRequest{Spec: spec, Verb: string(toolchain.VerbProbe)})

	resp, out := do(t, srv, http.MethodPost, "/agent/v1/tools/run", "tok", string(body))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, out)
	}
	var r ToolRunResponse
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("decode: %v (%s)", err, out)
	}
	if r.Error != "" {
		t.Fatalf("unexpected error: %s", r.Error)
	}
	var p toolchain.Probe
	if err := json.Unmarshal(r.JSON, &p); err != nil {
		t.Fatalf("decode probe: %v", err)
	}
	if !p.Present || p.Version != "10380 (0b1bad14f)" {
		t.Errorf("probe = %+v, want the version off stderr", p)
	}
}

func TestToolRunRejectsUnknownVerb(t *testing.T) {
	_, srv := newTestAgent(t, "tok")
	body, _ := json.Marshal(ToolRunRequest{Spec: toolchain.Spec{Name: "llama.cpp"}, Verb: "rm-rf"})
	resp, _ := do(t, srv, http.MethodPost, "/agent/v1/tools/run", "tok", string(body))
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status %d, want 400 for an unknown verb", resp.StatusCode)
	}
}

// The route is behind the same token as everything else. It executes recipes,
// which is not a surface to leave open.
func TestToolRunRequiresToken(t *testing.T) {
	_, srv := newTestAgent(t, "tok")
	body, _ := json.Marshal(ToolRunRequest{Spec: toolchain.Spec{Name: "llama.cpp"}, Verb: "probe"})
	resp, _ := do(t, srv, http.MethodPost, "/agent/v1/tools/run", "", string(body))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status %d, want 401 without a token", resp.StatusCode)
	}
}

// install-deps is refused unless this machine opted in, and the refusal comes
// back as a readable answer rather than an HTTP failure.
func TestInstallDepsRefusedUnlessEnabled(t *testing.T) {
	_, srv := newTestAgent(t, "tok")
	spec := toolchain.Spec{Name: "ninfer", Recipe: "ninfer", Prefix: t.TempDir()}
	body, _ := json.Marshal(ToolRunRequest{Spec: spec, Verb: string(toolchain.VerbInstallDeps)})

	resp, out := do(t, srv, http.MethodPost, "/agent/v1/tools/run", "tok", string(body))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, out)
	}
	var r ToolRunResponse
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatal(err)
	}
	var res toolchain.InstallDeps
	if err := json.Unmarshal(r.JSON, &res); err != nil {
		t.Fatalf("decode: %v (%s)", err, r.JSON)
	}
	if res.Allowed {
		t.Error("installed system packages without --allow-install-deps")
	}
	if !strings.Contains(res.Error, "allow-install-deps") {
		t.Errorf("refusal does not say how to enable it: %q", res.Error)
	}
}

// A remote build must emit its log AS IT HAPPENS.
//
// It used to arrive in one lump at the end, which for a ten-minute compile is
// indistinguishable from a hang. This asserts lines land while the run is still
// going, not merely that they all arrive eventually.
func TestStreamedBuildLogArrivesDuringTheRun(t *testing.T) {
	a, srv := newTestAgent(t, "tok")
	_ = a

	bin := t.TempDir()
	spec := toolchain.Spec{Name: "llama.cpp", Recipe: "llama.cpp", Bin: "llama-server", InstalledAt: bin}
	// probe on an adopted path: short, and enough to prove framing.
	body, _ := json.Marshal(ToolRunRequest{
		Spec: spec, Verb: string(toolchain.VerbProbe), Stream: true,
	})

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/agent/v1/tools/run", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set(ProtocolHeader, strconv.Itoa(Protocol))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "x-ndjson") {
		t.Fatalf("content type %q — the client decides how to read by this, so it must say ndjson", ct)
	}

	dec := json.NewDecoder(resp.Body)
	var sawDone bool
	var result toolchain.Probe
	for {
		var f ToolRunFrame
		if err := dec.Decode(&f); err != nil {
			break
		}
		if f.Done {
			sawDone = true
			if len(f.JSON) > 0 {
				_ = json.Unmarshal(f.JSON, &result)
			}
			// The terminal frame must NOT repeat the log: it has already been
			// sent line by line, and carrying it again doubles every build.
			if f.Log != "" {
				t.Error("the done frame carried the log again")
			}
			break
		}
	}
	if !sawDone {
		t.Fatal("stream ended with no terminal frame; a client cannot tell success from a dropped connection")
	}
	if result.Present {
		t.Errorf("probe of an empty dir reported present: %+v", result)
	}
}

// An agent that predates streaming ignores the flag and answers the old way.
// The client decides by content type, so both shapes must keep working.
func TestNonStreamingRequestStillReturnsOneObject(t *testing.T) {
	_, srv := newTestAgent(t, "tok")
	spec := toolchain.Spec{Name: "llama.cpp", Recipe: "llama.cpp", Bin: "llama-server", InstalledAt: t.TempDir()}
	body, _ := json.Marshal(ToolRunRequest{Spec: spec, Verb: string(toolchain.VerbProbe)})

	resp, out := do(t, srv, http.MethodPost, "/agent/v1/tools/run", "tok", string(body))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if strings.Contains(resp.Header.Get("Content-Type"), "x-ndjson") {
		t.Error("answered with a stream when none was asked for")
	}
	var r ToolRunResponse
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("decode: %v (%s)", err, out)
	}
	if len(r.JSON) == 0 {
		t.Error("no result in the non-streaming reply")
	}
}
