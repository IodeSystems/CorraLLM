package greet

import "testing"

func TestGreet(t *testing.T) {
	if got := Greet("Bob"); got != "Hello, Bob!" {
		t.Fatalf("Greet(%q) = %q, want %q", "Bob", got, "Hello, Bob!")
	}
}
