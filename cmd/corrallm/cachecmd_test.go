package main

import (
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
)

func TestResolveLocalModel(t *testing.T) {
	cfg := &config.Config{Models: map[string]config.Model{
		"local-Swift-27b": {Cmd: "llama-server"},
		"local-qwen-a":    {Cmd: "llama-server"},
		"local-qwen-b":    {Cmd: "llama-server"},
		"remote-swift":    {}, // no cmd: not local
	}}
	for _, c := range []struct {
		in, want string
		err      bool
	}{
		{"local-qwen-a", "local-qwen-a", false}, // exact
		{"swift", "local-Swift-27b", false},     // one local match, any case
		{"qwen", "", true},                      // two
		{"nothing", "", true},
	} {
		got, _, err := resolveLocalModel(cfg, c.in)
		if got != c.want || (err != nil) != c.err {
			t.Errorf("%q → %q, %v", c.in, got, err)
		}
	}
}
