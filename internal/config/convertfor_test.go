package config

import "testing"

// ConvertFor resolves a served name to the model whose convert block applies:
// the model itself, a lane's first member, or the model an alias names.
func TestConvertForResolvesNameLaneAndAlias(t *testing.T) {
	c := &Config{
		Models: map[string]Model{
			"swift": {Aliases: []string{"qwen"}, Convert: &ConvertConfig{PDF: "text"}},
			"ocr":   {Convert: &ConvertConfig{PDF: "vision"}},
			"plain": {},
		},
		Lanes: map[string]Lane{"read": {Members: []LaneMember{{Model: "ocr"}}}},
	}
	global := ConvertConfig{PDF: "off"}
	for served, want := range map[string]string{
		"swift":   "text",   // the model's own block
		"qwen":    "text",   // an alias gets its canonical model's block
		"read":    "vision", // a lane gets its first member's block
		"plain":   "off",    // no block: the global strategy
		"unknown": "off",    // unresolvable: the global strategy
	} {
		if got := c.ConvertFor(global, served).PDF; got != want {
			t.Errorf("ConvertFor(%q).PDF = %q, want %q", served, got, want)
		}
	}
}
