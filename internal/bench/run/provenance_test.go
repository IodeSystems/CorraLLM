package run

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A bench result is a claim about a specific build. The daemon spawned a
// day-old llm-bench that silently ignored the probeDirs key it did not
// understand — it would have run sixteen built-in probes, reported a full run,
// and been right about everything except what it measured.
func TestStampReadsBuildInfoFromAGoBinary(t *testing.T) {
	// `go test` binaries carry build info but NO vcs settings — the toolchain
	// does not stamp them. So this asserts what is guaranteed here (build info
	// readable, size and mtime recorded) and leaves the revision path to
	// TestStampReadsRevisionFromARealBuild, which needs a real binary.
	self, err := os.Executable()
	if err != nil {
		t.Skip("no executable path")
	}
	s := stampBinary("self", self)
	if s.Err != "" {
		t.Errorf("a Go binary should yield build info, got error %q", s.Err)
	}
	if s.Size == 0 || s.ModTime == "" {
		t.Error("size and mtime are the fallback identity and must always be recorded")
	}
}

// The revision path, against a binary the toolchain really stamped. Skipped
// rather than failed when the artifact is absent: this asserts a property of
// debug/buildinfo, and a missing build directory is not evidence against it.
func TestStampReadsRevisionFromARealBuild(t *testing.T) {
	for _, p := range []string{"../../../local/bin/llm-bench", "../../../local/bin/llm-bench-mcp"} {
		abs, err := filepath.Abs(p)
		if err != nil {
			continue
		}
		if _, err := os.Stat(abs); err != nil {
			continue
		}
		s := stampBinary("llm-bench", abs)
		if s.Revision == "" {
			t.Errorf("%s carries no revision (err=%q) — the stamp cannot identify it", abs, s.Err)
			return
		}
		if !strings.Contains(s.String(), shortRev(s.Revision)) {
			t.Errorf("the rendered line must carry the revision: %q", s.String())
		}
		return
	}
	t.Skip("no built binary to read; run `go build -o local/bin/llm-bench ./cmd/llm-bench`")
}

// A run that cannot identify what it measured must SAY so, not abort and not
// pretend. The measurement is still worth having; it is just worth trusting
// less, and the reader needs to know which.
func TestUnidentifiableBinariesAreSaidSoNotSwallowed(t *testing.T) {
	missing := stampBinary("ghost", filepath.Join(t.TempDir(), "not-here"))
	if missing.Err == "" {
		t.Error("a missing binary must record why it could not be stamped")
	}
	if !strings.Contains(missing.String(), "UNIDENTIFIED") {
		t.Errorf("rendered as %q, want it to say UNIDENTIFIED", missing.String())
	}

	// A non-Go file: readable, but carries no build info.
	p := filepath.Join(t.TempDir(), "script.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	sh := stampBinary("script", p)
	if sh.Revision != "" {
		t.Error("a shell script has no revision to report")
	}
	if sh.Size == 0 || sh.ModTime == "" {
		t.Error("size and mtime are the fallback identity and must be present")
	}
	if !strings.Contains(sh.String(), "UNIDENTIFIED") {
		t.Errorf("rendered as %q, want UNIDENTIFIED", sh.String())
	}
}

// A number measured against uncommitted work cannot be reproduced by anyone,
// including the person who produced it. It has to be loud.
func TestDirtyTreeIsLoud(t *testing.T) {
	p := Provenance{
		LLMBench: BinStamp{Name: "llm-bench", Revision: "abc123abc123def", Path: "/x"},
		Toolsets: map[string][]BinStamp{
			"mcpshell": {{Name: "mcpshell", Revision: "0ce5188b4f51", Modified: true, Path: "/y"}},
		},
	}
	if !p.Dirty() {
		t.Error("a modified toolset binary must make the run dirty")
	}
	joined := strings.Join(p.Lines(), "\n")
	if !strings.Contains(joined, "DIRTY TREE") {
		t.Errorf("the dirty build must be marked in the log:\n%s", joined)
	}
	if !strings.Contains(joined, "toolset mcpshell") {
		t.Errorf("the line must name which toolset:\n%s", joined)
	}
	// A clean run says nothing alarming.
	clean := Provenance{LLMBench: BinStamp{Name: "llm-bench", Revision: "abc123abc123"}}
	if clean.Dirty() {
		t.Error("a clean run must not be reported dirty")
	}
	if strings.Contains(strings.Join(clean.Lines(), "\n"), "DIRTY") {
		t.Error("a clean run must not mention dirt")
	}
}

// The stamp must name the file that RUNS, not the string someone typed —
// resolved through the same resolveCmd the runner uses.
func TestProvenanceStampsResolvedToolsetPaths(t *testing.T) {
	binDir := t.TempDir()
	fake := filepath.Join(binDir, "toolsrv")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := collectProvenance([]Toolset{
		{Name: "withtool", Servers: []ServerSpec{{Cmd: "toolsrv"}}},
		{Name: "baseline"},
	}, binDir, "")

	got, ok := p.Toolsets["withtool"]
	if !ok || len(got) != 1 {
		t.Fatalf("toolsets = %+v, want one stamp under withtool", p.Toolsets)
	}
	if got[0].Path != fake {
		t.Errorf("path = %q, want the RESOLVED %q — a bare name resolves differently per cwd",
			got[0].Path, fake)
	}
	if _, ok := p.Toolsets["baseline"]; ok {
		t.Error("a toolset with no servers has nothing to stamp")
	}
}

// A run directory has to be self-describing after the log scrolls away.
func TestProvenanceWritesBesideTheResults(t *testing.T) {
	dir := t.TempDir()
	p := Provenance{LLMBench: BinStamp{Name: "llm-bench", Revision: "deadbeefcafe"}}
	if err := p.Write(dir); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "provenance.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "deadbeefcafe") {
		t.Errorf("provenance.json missing the revision:\n%s", b)
	}
}
