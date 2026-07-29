package app

import "testing"

func TestBoot(t *testing.T) {
	if got := Boot(); got != ":8080 app" {
		t.Fatalf("Boot() = %q, want \":8080 app\"", got)
	}
	var c Config = Default()
	if c.Port != 8080 {
		t.Fatalf("Port = %d", c.Port)
	}
}
