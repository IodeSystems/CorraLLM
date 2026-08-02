package run

import (
	"debug/buildinfo"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
)

// Which binaries a run actually measured.
//
// A bench result is a claim about a specific build, and nothing in the output
// used to say which one. That is not a theoretical gap: the daemon spawned
// llm-bench from local/bin, which was a day old and silently ignored the
// probeDirs key it did not understand — it would have run sixteen built-in
// probes, reported them as a full run, and been right about everything except
// what it measured. The toolset binaries are worse, because there are three
// plausible snapshot locations for each (a repo's bin/, ~/.corrallm/bin,
// ~/go/bin) and none of them announce their age.
//
// Go binaries carry their own VCS stamp, so this costs nothing to collect and
// needs no ldflags: debug/buildinfo reads the revision straight out of an
// executable on disk. A dirty tree is stamped too, and said loudly — a number
// measured against uncommitted work is not reproducible by anyone, including
// the person who produced it.
type BinStamp struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Revision string `json:"revision,omitempty"`
	BuiltAt  string `json:"builtAt,omitempty"`
	Modified bool   `json:"modified,omitempty"`

	// Size and ModTime identify a build that carries no VCS stamp — a
	// cross-compiled binary, one built from a tarball, or something not written
	// in Go. Weaker than a revision and better than nothing.
	Size    int64  `json:"size,omitempty"`
	ModTime string `json:"modTime,omitempty"`

	// Err records why a stamp is thin, rather than leaving the reader to guess
	// whether the field is missing or the binary is.
	Err string `json:"error,omitempty"`
}

// Provenance is every binary that shaped a run's numbers.
type Provenance struct {
	LLMBench BinStamp              `json:"llmBench"`
	McpBin   BinStamp              `json:"mcpBin"`
	Toolsets map[string][]BinStamp `json:"toolsets,omitempty"`
}

// shortRev trims a git revision to the length a human compares.
func shortRev(r string) string {
	if len(r) > 12 {
		return r[:12]
	}
	return r
}

// stampBinary reads a binary's identity off disk. It never fails: an
// unreadable or unstamped binary produces a thin record saying so, because a
// run that cannot identify what it measured should say that rather than abort
// — the measurement is still worth having, it is just worth trusting less.
func stampBinary(name, path string) BinStamp {
	s := BinStamp{Name: name, Path: path}
	if fi, err := os.Stat(path); err == nil {
		s.Size = fi.Size()
		s.ModTime = fi.ModTime().UTC().Format("2006-01-02T15:04:05Z")
	} else {
		s.Err = err.Error()
		return s
	}
	bi, err := buildinfo.ReadFile(path)
	if err != nil {
		s.Err = "no build info: " + err.Error()
		return s
	}
	for _, kv := range bi.Settings {
		switch kv.Key {
		case "vcs.revision":
			s.Revision = kv.Value
		case "vcs.time":
			s.BuiltAt = kv.Value
		case "vcs.modified":
			s.Modified = kv.Value == "true"
		}
	}
	return s
}

// selfStamp identifies the running llm-bench from its own embedded build info.
func selfStamp(path string) BinStamp {
	s := BinStamp{Name: "llm-bench", Path: path}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		s.Err = "no build info (built without VCS?)"
		return s
	}
	for _, kv := range bi.Settings {
		switch kv.Key {
		case "vcs.revision":
			s.Revision = kv.Value
		case "vcs.time":
			s.BuiltAt = kv.Value
		case "vcs.modified":
			s.Modified = kv.Value == "true"
		}
	}
	return s
}

// collectProvenance stamps llm-bench, its MCP helper, and every toolset server
// that will actually be spawned — resolved through the SAME resolveCmd the
// runner uses, so the stamp names the file that runs rather than the string
// someone typed.
func collectProvenance(toolsets []Toolset, binDir, mcpBin string) Provenance {
	self, _ := os.Executable()
	p := Provenance{
		LLMBench: selfStamp(self),
		McpBin:   stampBinary("llm-bench-mcp", mcpBin),
	}
	for _, ts := range toolsets {
		for _, sv := range ts.Servers {
			path := resolveCmd(binDir, sv.Cmd)
			if p.Toolsets == nil {
				p.Toolsets = map[string][]BinStamp{}
			}
			// Basename for the label: a config may name a bare command or an
			// absolute path, and echoing the whole path twice on one line
			// buries the revision that is the point of it.
			p.Toolsets[ts.Name] = append(p.Toolsets[ts.Name],
				stampBinary(filepath.Base(sv.Cmd), path))
		}
	}
	return p
}

// Lines renders the provenance for a run log, one binary per line.
//
// Logged, not just written to a file: the log is what a person reads when a
// number surprises them, and "which build was that" is the first question. A
// dirty tree is marked, because a result measured against uncommitted work
// cannot be reproduced by anyone.
func (p Provenance) Lines() []string {
	out := []string{"llm-bench: " + p.LLMBench.String()}
	if p.McpBin.Path != "" {
		out = append(out, "llm-bench: "+p.McpBin.String())
	}
	for _, name := range sortedKeys(p.Toolsets) {
		for _, s := range p.Toolsets[name] {
			out = append(out, fmt.Sprintf("llm-bench: toolset %s → %s", name, s.String()))
		}
	}
	return out
}

// String is one binary's identity, in the form a person compares against
// `git log`.
func (s BinStamp) String() string {
	var b strings.Builder
	b.WriteString(s.Name)
	switch {
	case s.Revision != "":
		fmt.Fprintf(&b, " %s", shortRev(s.Revision))
		if s.BuiltAt != "" {
			fmt.Fprintf(&b, " (%s)", s.BuiltAt)
		}
	case s.Err != "":
		fmt.Fprintf(&b, " UNIDENTIFIED (%s)", s.Err)
	default:
		b.WriteString(" UNIDENTIFIED")
	}
	if s.Modified {
		b.WriteString("  ⚠ DIRTY TREE — built from uncommitted changes, not reproducible")
	}
	if s.Path != "" {
		fmt.Fprintf(&b, "  [%s]", s.Path)
	}
	return b.String()
}

// Dirty reports whether any measured binary came from an uncommitted tree.
func (p Provenance) Dirty() bool {
	if p.LLMBench.Modified || p.McpBin.Modified {
		return true
	}
	for _, stamps := range p.Toolsets {
		for _, s := range stamps {
			if s.Modified {
				return true
			}
		}
	}
	return false
}

// Write persists provenance.json beside the run's other artifacts, so a result
// directory is self-describing after the log has scrolled away.
func (p Provenance) Write(outDir string) error {
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(outDir, "provenance.json"), append(b, '\n'), 0o644)
}

func sortedKeys(m map[string][]BinStamp) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
