package config

import (
	"strings"
	"testing"
	"time"
)

func toolsCfg(t Tool) *Config {
	return &Config{
		Servers: map[string]Server{"box1": {}, "mac1": {}},
		Tools:   map[string]Tool{"llama.cpp": t},
	}
}

func validTool() Tool {
	return Tool{
		URL:   "https://github.com/ggml-org/llama.cpp.git",
		Ref:   "master",
		Bin:   "llama-server",
		Hosts: map[string]ToolHost{"box1": {}},
	}
}

func TestValidToolPasses(t *testing.T) {
	if err := toolsCfg(validTool()).validateTools(); err != nil {
		t.Fatalf("valid tool rejected: %v", err)
	}
}

func TestToolNeedsURLAndRef(t *testing.T) {
	noURL := validTool()
	noURL.URL = ""
	if err := toolsCfg(noURL).validateTools(); err == nil {
		t.Error("accepted a tool with no url — nothing could check it for drift")
	}

	noRef := validTool()
	noRef.Ref = ""
	err := toolsCfg(noRef).validateTools()
	if err == nil {
		t.Fatal("accepted a tool with no ref")
	}
	// The message has to explain why there is no default: the two tools shipped
	// disagree (master vs main), so guessing is wrong half the time and silently.
	if !strings.Contains(err.Error(), "ninfer") {
		t.Errorf("ref error should say why there is no default: %v", err)
	}
}

// A tool declared on a server that does not exist reads exactly like a host that
// is down, and sends someone to check the wrong machine.
func TestToolHostMustBeADeclaredServer(t *testing.T) {
	tl := validTool()
	tl.Hosts = map[string]ToolHost{"typo1": {}}
	err := toolsCfg(tl).validateTools()
	if err == nil {
		t.Fatal("accepted a tool on an undeclared server")
	}
	if !strings.Contains(err.Error(), "typo1") {
		t.Errorf("error should name the offending host: %v", err)
	}
}

func TestUnknownRecipeRejected(t *testing.T) {
	tl := validTool()
	tl.Recipe = "not-a-real-recipe"
	err := toolsCfg(tl).validateTools()
	if err == nil {
		t.Fatal("accepted a tool naming a recipe that does not exist")
	}
	if !strings.Contains(err.Error(), "available") {
		t.Errorf("error should list what recipes exist: %v", err)
	}
}

// installedAt adopts an install corrallm never writes to; prefix says where
// corrallm should build one. Both together is a contradiction, and silently
// honouring one would decide something the operator did not.
func TestInstalledAtAndPrefixAreMutuallyExclusive(t *testing.T) {
	tl := validTool()
	tl.Hosts = map[string]ToolHost{"box1": {InstalledAt: "/opt/llama", Prefix: "/srv/llama"}}
	if err := toolsCfg(tl).validateTools(); err == nil {
		t.Error("accepted installedAt together with prefix")
	}
}

func TestAdoptedReportsInstalledAt(t *testing.T) {
	if !(ToolHost{InstalledAt: "/opt/x"}).Adopted() {
		t.Error("an entry with installedAt is adopted")
	}
	if (ToolHost{}).Adopted() {
		t.Error("an entry without installedAt is managed, not adopted")
	}
	if (ToolHost{InstalledAt: "  "}).Adopted() {
		t.Error("whitespace is not a path")
	}
}

func TestRecipeDefaultsToTheToolName(t *testing.T) {
	if got := RecipeOf("ninfer", Tool{}); got != "ninfer" {
		t.Errorf("RecipeOf = %q, want ninfer", got)
	}
	if got := RecipeOf("my-llama", Tool{Recipe: "llama.cpp"}); got != "llama.cpp" {
		t.Errorf("an explicit recipe must win, got %q", got)
	}
}

func TestCheckIntervalDefaultsOnAndCanBeDisabled(t *testing.T) {
	d, ok := CheckIntervalOf(Tool{})
	if !ok {
		t.Error("drift checking must default ON — one ls-remote is cheap and a rotting pin is invisible")
	}
	if d != DefaultCheckInterval {
		t.Errorf("default interval = %v, want %v", d, DefaultCheckInterval)
	}

	if _, ok := CheckIntervalOf(Tool{Check: "off"}); ok {
		t.Error(`check: "off" must disable it`)
	}
	if _, ok := CheckIntervalOf(Tool{Check: "0"}); ok {
		t.Error(`check: "0" must disable it`)
	}

	d, ok = CheckIntervalOf(Tool{Check: "90m"})
	if !ok || d != 90*time.Minute {
		t.Errorf("explicit interval = %v %v, want 90m true", d, ok)
	}
}

func TestBadCheckDurationRejected(t *testing.T) {
	tl := validTool()
	tl.Check = "every so often"
	if err := toolsCfg(tl).validateTools(); err == nil {
		t.Error("accepted a check interval that is not a duration")
	}
}

// rebuild is driven by the check, so rebuild-on with check-off is a setting that
// can never fire. Saying so beats silently never rebuilding.
func TestRebuildWithoutCheckIsRejected(t *testing.T) {
	tl := validTool()
	tl.Rebuild = true
	tl.Check = "off"
	err := toolsCfg(tl).validateTools()
	if err == nil {
		t.Fatal("accepted rebuild:true with check:off")
	}
	if !strings.Contains(err.Error(), "never fire") {
		t.Errorf("error should say the setting is inert: %v", err)
	}
}

// Rebuilding defaults OFF: a CUDA build is ten to twenty minutes of pegged GPU
// that can replace a binary running models depend on.
func TestRebuildDefaultsOff(t *testing.T) {
	if (Tool{}).Rebuild {
		t.Error("scheduled rebuild must be opt-in")
	}
}

// Nothing about tools may become load-bearing: a config with no tools: block is
// the normal state and must validate.
func TestNoToolsIsFine(t *testing.T) {
	c := &Config{Servers: map[string]Server{"box1": {}}}
	if err := c.validateTools(); err != nil {
		t.Errorf("a config with no tools must validate: %v", err)
	}
}
