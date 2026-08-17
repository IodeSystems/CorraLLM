package toolchain

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/iodesystems/corrallm/internal/toolchain/recipes"
)

// Raw is one recipe invocation's result: the JSON object it printed, and
// everything it said on the way there.
//
// Both are kept because they answer different questions. The JSON is the
// machine's answer; the log is what an operator needs when the answer is "no"
// and the reason is three lines up in a cmake error.
type Raw struct {
	JSON json.RawMessage `json:"json"`
	Log  string          `json:"log"`
}

// Runner executes a recipe verb somewhere — this machine, or a host reached
// through its agent.
//
// The seam is the same one host.Host draws for backends, and for the same
// reason: everything above it is host-agnostic, and there is exactly one
// implementation of each side rather than two that drift.
type Runner interface {
	// Where names the server this runner acts on, for error messages.
	Where() string
	Run(ctx context.Context, spec Spec, verb Verb) (*Raw, error)
}

// Timeout bounds one verb.
//
// Per-verb rather than one number, because the range is four orders of
// magnitude and a single timeout is necessarily wrong at one end: generous
// enough for a build, it lets a wedged probe hang the scheduled sweep; tight
// enough for a probe, it kills every install.
func Timeout(v Verb) time.Duration {
	switch v {
	case VerbProbe, VerbPreflight:
		return 30 * time.Second
	case VerbUpstream:
		// One `git ls-remote` over whatever network the host has.
		return 90 * time.Second
	case VerbInstallDeps:
		return 15 * time.Minute
	case VerbBuild:
		return 2 * time.Hour
	default:
		return time.Minute
	}
}

// Local runs recipes on this machine. It backs both the primary's own host and
// the agent's side of a remote call.
type Local struct {
	// Dir is where recipes are extracted and run from. Empty uses a directory
	// under the OS temp dir.
	Dir string
	// AllowInstallDeps gates the one verb that mutates the system.
	//
	// The agent already runs arbitrary shell by design, so this is not much of a
	// security boundary — if passwordless sudo exists, a backend cmd could
	// already use it. What it buys is a promise: corrallm does not touch system
	// packages unless somebody said it may, on this host, deliberately.
	AllowInstallDeps bool
	// Server is the name this machine answers to, for error messages.
	Server string
}

func (l *Local) Where() string {
	if l.Server == "" {
		return "local"
	}
	return l.Server
}

func (l *Local) dir() string {
	if l.Dir != "" {
		return l.Dir
	}
	return filepath.Join(os.TempDir(), "corrallm-recipes")
}

// Run extracts the recipe set and executes one verb.
func (l *Local) Run(ctx context.Context, spec Spec, verb Verb) (*Raw, error) {
	recipe := spec.Recipe
	if recipe == "" {
		recipe = spec.Name
	}
	if !recipes.Has(recipe) {
		return nil, fmt.Errorf("no recipe %q on %s (have: %s)",
			recipe, l.Where(), strings.Join(recipes.Names(), ", "))
	}
	if verb == VerbInstallDeps && !l.AllowInstallDeps {
		// A refusal, not a failure — and it returns a well-formed result so the
		// caller can report "not allowed here" rather than "something went
		// wrong". The commands to run by hand are already in Preflight.
		b, _ := json.Marshal(InstallDeps{
			OK:      false,
			Allowed: false,
			Error:   "installing system packages is not enabled on " + l.Where() + " (start the agent with --allow-install-deps)",
		})
		return &Raw{JSON: b}, nil
	}

	dir := l.dir()
	// Re-extracted every call: cheap, idempotent, and it keeps a stale
	// extraction from outliving an agent self-update.
	if err := recipes.Extract(dir); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, Timeout(verb))
	defer cancel()

	script := filepath.Join(dir, recipe+".sh")
	cmd := exec.CommandContext(ctx, "bash", script, string(verb))
	cmd.Env = specEnv(spec)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb

	runErr := cmd.Run()
	raw := &Raw{Log: errb.String()}

	// The JSON is the LAST non-empty stdout line. A recipe that writes progress
	// to stdout by mistake is a bug in that recipe, but reading the last line
	// keeps one stray echo from turning every answer into a parse error.
	if line := lastJSONLine(out.String()); line != "" {
		raw.JSON = json.RawMessage(line)
	}

	if raw.JSON == nil {
		if ctx.Err() != nil {
			return raw, fmt.Errorf("recipe %s %s on %s timed out after %s", recipe, verb, l.Where(), Timeout(verb))
		}
		if runErr != nil {
			return raw, fmt.Errorf("recipe %s %s on %s: %w", recipe, verb, l.Where(), runErr)
		}
		return raw, fmt.Errorf("recipe %s %s on %s printed no JSON result", recipe, verb, l.Where())
	}
	// A non-zero exit WITH valid JSON is the recipe's own error path (`die`),
	// which the caller reads out of the object. Not an error here.
	return raw, nil
}

// specEnv builds the recipe environment.
//
// The process environment is inherited rather than replaced: a recipe needs
// PATH to find git and cmake, HOME for git's config, and CUDA_HOME/CUDA_VERSION
// are honoured by the ninfer recipe exactly as ml-kit's builder honours them.
func specEnv(spec Spec) []string {
	env := os.Environ()
	set := func(k, v string) { env = append(env, k+"="+v) }
	set("TOOL_NAME", spec.Name)
	set("TOOL_URL", spec.URL)
	set("TOOL_REF", spec.Ref)
	set("TOOL_BIN", spec.Bin)
	set("TOOL_PREFIX", spec.Prefix)
	set("TOOL_INSTALLED_AT", spec.InstalledAt)
	return env
}

func lastJSONLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		t := strings.TrimSpace(lines[i])
		if strings.HasPrefix(t, "{") && strings.HasSuffix(t, "}") {
			return t
		}
	}
	return ""
}

// RunProbe asks what is installed.
func RunProbe(ctx context.Context, r Runner, spec Spec) (*Probe, error) {
	return runTyped[Probe](ctx, r, spec, VerbProbe)
}

// RunUpstream asks whether the pin has moved.
func RunUpstream(ctx context.Context, r Runner, spec Spec) (*Upstream, error) {
	return runTyped[Upstream](ctx, r, spec, VerbUpstream)
}

// RunPreflight asks whether this host could build it.
func RunPreflight(ctx context.Context, r Runner, spec Spec) (*Preflight, error) {
	return runTyped[Preflight](ctx, r, spec, VerbPreflight)
}

// RunInstallDeps installs what preflight found missing. Operator-triggered
// only — nothing schedules this.
func RunInstallDeps(ctx context.Context, r Runner, spec Spec) (*InstallDeps, error) {
	return runTyped[InstallDeps](ctx, r, spec, VerbInstallDeps)
}

func runTyped[T any](ctx context.Context, r Runner, spec Spec, verb Verb) (*T, error) {
	raw, err := r.Run(ctx, spec, verb)
	if err != nil {
		return nil, err
	}
	var v T
	if err := json.Unmarshal(raw.JSON, &v); err != nil {
		return nil, fmt.Errorf("recipe %s %s on %s returned unparseable JSON: %w", spec.Name, verb, r.Where(), err)
	}
	// A recipe's own error path is a valid object carrying `error`. Surface it
	// as an error so a caller cannot mistake it for a successful answer, and
	// still hand back the value for anything that wants the detail.
	if e := errorField(raw.JSON); e != "" {
		return &v, fmt.Errorf("%s", e)
	}
	return &v, nil
}

func errorField(b []byte) string {
	var probe struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		return ""
	}
	return probe.Error
}
