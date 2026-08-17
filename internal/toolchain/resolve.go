package toolchain

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// ${tool:<name>} in a model's cmd resolves, at spawn time, to that tool's
// install directory ON THE HOST THE MODEL RUNS ON.
//
// The problem it solves is visible in the config it replaces: the same binary is
// spelled /home/nthalk/local/src/ml-kit/local/bin/llama.cpp on box1 and
// /Users/nthalk/... on the Mac, hand-maintained in every model that uses it.
// One reference is correct on both, and a rebuild is picked up on the next spawn
// without editing anything.
var toolRef = regexp.MustCompile(`\$\{tool:([A-Za-z0-9._+-]+)\}`)

// HasToolRef reports whether a command needs expansion. Cheap enough to call on
// every spawn, which is the point: nothing pays for this feature unless it uses
// it, and every existing absolute-path cmd keeps working untouched.
func HasToolRef(cmd string) bool { return toolRef.MatchString(cmd) }

// resolved caches a (tool, host) → directory answer.
//
// Only SUCCESSFUL resolutions are cached. Caching "not installed" would make a
// tool stay unusable for the life of the daemon after it was built, which is the
// opposite of the point.
type resolvedKey struct{ tool, host string }

// ExpandTools replaces every ${tool:x} in cmd with x's install directory on
// host, and REFUSES rather than guessing.
//
// A reference that cannot be resolved fails the spawn with the reason. Falling
// back to PATH would run whichever llama-server the machine happens to have,
// which is precisely the ambiguity this whole phase exists to remove — and it
// would do so silently, at the moment a model is being loaded, where the wrong
// binary looks like a model bug.
func (r *Registry) ExpandTools(ctx context.Context, cmd, host string) (string, error) {
	if !HasToolRef(cmd) {
		return cmd, nil
	}
	var firstErr error
	out := toolRef.ReplaceAllStringFunc(cmd, func(m string) string {
		name := toolRef.FindStringSubmatch(m)[1]
		dir, err := r.ToolDir(ctx, name, host)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			return m
		}
		return dir
	})
	if firstErr != nil {
		return "", firstErr
	}
	return out, nil
}

// ToolDir is where a tool's binaries live on a host.
//
// Two paths, and the difference matters. An ADOPTED entry states its directory
// in config, so it resolves with no round trip at all. A MANAGED entry is asked
// of the host, because only the host knows where its own install actually
// landed — the primary's idea of a prefix is its own home directory, which is
// not the agent's on a machine whose home is /Users rather than /home.
func (r *Registry) ToolDir(ctx context.Context, tool, host string) (string, error) {
	spec, declared, err := r.SpecFor(tool, host)
	if err != nil {
		return "", fmt.Errorf("${tool:%s}: %w", tool, err)
	}
	if !declared {
		return "", fmt.Errorf("${tool:%s} is not declared on host %q — add it under tools.%s.hosts", tool, host, tool)
	}
	if spec.InstalledAt != "" {
		return strings.TrimRight(spec.InstalledAt, "/"), nil
	}

	key := resolvedKey{tool, host}
	r.mu.Lock()
	if dir, ok := r.resolved[key]; ok {
		r.mu.Unlock()
		return dir, nil
	}
	r.mu.Unlock()

	runner, err := r.RunnerFor(host)
	if err != nil {
		return "", fmt.Errorf("${tool:%s} on %q: %w", tool, host, err)
	}
	p, err := RunProbe(ctx, runner, spec)
	if err != nil {
		return "", fmt.Errorf("${tool:%s} on %q: could not ask the host what is installed: %w", tool, host, err)
	}
	if !p.Present {
		return "", fmt.Errorf("${tool:%s} on %q is not built yet (expected %s) — run `corrallm tools build %s --server %s`",
			tool, host, p.Path, tool, host)
	}
	dir := filepath.Dir(p.Path)

	r.mu.Lock()
	if r.resolved == nil {
		r.resolved = map[resolvedKey]string{}
	}
	r.resolved[key] = dir
	r.mu.Unlock()
	return dir, nil
}

// InvalidateResolved drops a cached directory, so the next spawn re-asks.
//
// Called after a build: the path is normally stable across builds, but a build
// is also the moment a previously-absent tool becomes present, and that
// transition is exactly the one worth not remembering wrongly.
func (r *Registry) InvalidateResolved(tool, host string) {
	r.mu.Lock()
	delete(r.resolved, resolvedKey{tool, host})
	r.mu.Unlock()
}
