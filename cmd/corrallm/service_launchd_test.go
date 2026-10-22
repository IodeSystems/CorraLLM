package main

import "testing"

func TestLocalBase(t *testing.T) {
	for in, want := range map[string]string{
		"":              "http://127.0.0.1:6502",
		":6502":         "http://127.0.0.1:6502",
		"0.0.0.0:8111":  "http://127.0.0.1:8111",
		"[::]:8111":     "http://127.0.0.1:8111",
		"10.0.0.5:8111": "http://10.0.0.5:8111",
	} {
		if got := localBase(in); got != want {
			t.Errorf("localBase(%q) = %q, want %q", in, got, want)
		}
	}
}
