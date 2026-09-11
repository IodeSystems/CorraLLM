package toolchain

import (
	"fmt"
	"strings"
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
)

func TestUsersOfNamesWhoStartsFromEachTool(t *testing.T) {
	cfg := &config.Config{
		Models: map[string]config.Model{
			"qwen":   {Cmd: `exec ${tool:llama.cpp}/llama-server -hf x`},
			"ocr":    {Cmd: `exec ${tool:llama.cpp}/llama-server -hf y`},
			"remote": {}, // a proxy: no cmd, starts from nothing
		},
		Extensions: map[string]config.Extension{
			"oidio": {Cmd: `exec ${tool:ninfer}/ninfer-serve`},
		},
	}
	got := UsersOf(cfg)
	if want := []string{"ocr", "qwen"}; fmt.Sprint(got["llama.cpp"]) != fmt.Sprint(want) {
		t.Errorf("llama.cpp users = %v, want %v (sorted)", got["llama.cpp"], want)
	}
	// An extension starts from a tool exactly as a model does — it is one
	// process serving several models, and missing it would report a tool as
	// unused while a process is running from it.
	if fmt.Sprint(got["ninfer"]) != fmt.Sprint([]string{"oidio"}) {
		t.Errorf("ninfer users = %v, want [oidio]", got["ninfer"])
	}
	// Absent, not empty: nothing references it, and the caller distinguishes
	// "no users" from "tool not mentioned" by presence.
	if _, present := got["vllm"]; present {
		t.Error("a tool nobody references appeared in the map")
	}
}

func TestUsersOfOnNothing(t *testing.T) {
	if got := UsersOf(nil); got != nil {
		t.Errorf("got %v, want nil for a nil config", got)
	}
	if got := UsersOf(&config.Config{}); len(got) != 0 {
		t.Errorf("got %v, want empty", got)
	}
}

// The cost question, answered rather than assumed: this is a regexp over cmd
// strings already in memory, with no I/O and nothing probed, so it can be
// computed per request instead of cached and invalidated.
func BenchmarkUsersOf(b *testing.B) {
	// The shape this box actually runs: 12 models, one with a ~2KB quant-ladder
	// shell script for a cmd, plus three extensions.
	big := "# a quant ladder\n" + strings.Repeat("echo picking a rung >&2\n", 60) +
		`exec ${tool:llama.cpp}/llama-server -hf unsloth/Qwen3.8-27B-GGUF --port 5802`
	cfg := &config.Config{Models: map[string]config.Model{}, Extensions: map[string]config.Extension{}}
	for i := 0; i < 12; i++ {
		cfg.Models[fmt.Sprintf("m%d", i)] = config.Model{Cmd: big}
	}
	for i := 0; i < 3; i++ {
		cfg.Extensions[fmt.Sprintf("e%d", i)] = config.Extension{Cmd: `exec ${tool:ninfer}/ninfer-serve`}
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = UsersOf(cfg)
	}
}
