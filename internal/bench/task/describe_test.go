package task

import (
	"os"
	"strings"
	"testing"
)

func TestFirstLineSkipsMarkdownFurniture(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"heading first", "## What this measures\n\nWhether a model resists bait.\n",
			"Whether a model resists bait."},
		{"h1 and blanks", "# Title\n\n\n## Section\n\nReal prose here.\n", "Real prose here."},
		{"table first", "| a | b |\n|---|---|\n| 1 | 2 |\n\nProse after a table.\n",
			"Prose after a table."},
		{"bullets first", "- one\n- two\n\nProse after bullets.\n", "Prose after bullets."},
		{"quote first", "> quoted\n\nProse after a quote.\n", "Prose after a quote."},
		{"plain prose", "Just a sentence.\nAnd another.\n", "Just a sentence."},
		{"only furniture", "## Heading\n\n- a\n- b\n", ""},
		{"empty", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := firstLine(c.in); got != c.want {
				t.Errorf("firstLine(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestFirstLineCaps160(t *testing.T) {
	got := firstLine(strings.Repeat("x", 500))
	if len(got) != 160 {
		t.Fatalf("want 160 chars, got %d", len(got))
	}
}

// Every built-in probe must describe itself.
//
// A probe with no description is indistinguishable, in the catalog, from one
// whose description failed to parse — and the catalog exists precisely to end
// that kind of silence. This is the check that stops a new probe shipping
// undocumented.
func TestBuiltinProbesAreDescribed(t *testing.T) {
	entries, err := Catalog("", os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("no built-in probes resolved")
	}
	for _, e := range entries {
		if e.Error != "" {
			t.Errorf("%s failed to load: %s", e.Name, e.Error)
			continue
		}
		if strings.TrimSpace(e.Description) == "" {
			t.Errorf("%s has no description", e.Name)
		}
		// A summary that is a markdown heading means firstLine regressed.
		if strings.HasPrefix(e.Summary, "#") || strings.HasPrefix(e.Summary, "|") {
			t.Errorf("%s summary is markdown furniture, not prose: %q", e.Name, e.Summary)
		}
		if strings.TrimSpace(e.Summary) == "" {
			t.Errorf("%s has no summary", e.Name)
		}
	}
}
