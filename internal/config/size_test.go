package config

import "testing"

func TestParseSize(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"1024", 1024},
		{"1B", 1},
		{"2KB", 2000},
		{"2KiB", 2048},
		{"1MB", 1_000_000},
		{"1MiB", 1 << 20},
		{"32GB", 32_000_000_000},
		{"16GiB", 16 << 30},
		{"0.5GiB", 1 << 29},
		{"24G", 24 << 30},
	}
	for _, c := range cases {
		got, err := ParseSize(c.in)
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("%q: got %d want %d", c.in, got, c.want)
		}
	}
}

func TestParseSizeErrors(t *testing.T) {
	for _, in := range []string{"", "abc", "12XB", "GB"} {
		if _, err := ParseSize(in); err == nil {
			t.Errorf("%q: expected error", in)
		}
	}
}

func TestValidateRejectsUndeclaredPool(t *testing.T) {
	c := &Config{
		Servers: map[string]Server{"box": {Pools: map[string]string{"gpu0": "24GB"}}},
		Models: map[string]Model{
			"m": {Cmd: "x", Server: "box", RAMUsage: map[string]string{"gpu9": "8GB"}},
		},
	}
	if err := c.Validate(); err == nil {
		t.Fatal("expected error for ramUsage on undeclared pool gpu9")
	}
}
