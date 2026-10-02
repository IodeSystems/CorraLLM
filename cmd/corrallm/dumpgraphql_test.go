package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDumpGraphQLMarksFileGenerated: the committed snapshot says it is
// generated, so nobody edits it by hand and loses the edit on the next gen.
func TestDumpGraphQLMarksFileGenerated(t *testing.T) {
	out := filepath.Join(t.TempDir(), "schema.graphql")
	cmd := newDumpGraphQLCmd()
	cmd.SetArgs([]string{out})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	first := strings.SplitN(string(b), "\n", 2)[0]
	if !strings.HasPrefix(first, "# Code generated ") || !strings.HasSuffix(first, "DO NOT EDIT.") {
		t.Errorf("first line = %q, want a 'Code generated ... DO NOT EDIT.' header", first)
	}
	if !strings.Contains(string(b), "schema {") {
		t.Error("the SDL itself is missing after the header")
	}
}
