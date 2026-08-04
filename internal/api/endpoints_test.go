package api

import "testing"

// Interface enumeration order is not stable across beats. Comparing as ordered
// lists would see a reshuffle as a change and rewrite the config — on a hot path
// that runs once per agent per ten seconds.
func TestSameEndpointsIgnoresOrder(t *testing.T) {
	a := []string{"http://192.168.1.58:6503", "http://10.4.0.9:6503"}
	b := []string{"http://10.4.0.9:6503", "http://192.168.1.58:6503"}
	if !sameEndpoints(a, b) {
		t.Error("a reordered list was treated as changed; config would be rewritten on every beat")
	}
}

// The case the whole change exists for: the laptop moved.
func TestSameEndpointsDetectsARealMove(t *testing.T) {
	cases := []struct {
		name string
		a, b []string
		same bool
	}{
		{"new address", []string{"http://192.168.1.58:6503"}, []string{"http://192.168.5.20:6503"}, false},
		{"joined a VPN", []string{"http://192.168.1.58:6503"},
			[]string{"http://192.168.1.58:6503", "http://10.4.0.9:6503"}, false},
		{"left a network", []string{"http://192.168.1.58:6503", "http://10.4.0.9:6503"},
			[]string{"http://192.168.1.58:6503"}, false},
		// A dynamic port is a different endpoint even on the same host, which is
		// what makes `addr: ":0"` usable at all.
		{"port changed after restart", []string{"http://192.168.1.58:6503"},
			[]string{"http://192.168.1.58:54321"}, false},
		{"unchanged", []string{"http://192.168.1.58:6503"}, []string{"http://192.168.1.58:6503"}, true},
		{"both empty", nil, nil, true},
	}
	for _, c := range cases {
		if got := sameEndpoints(c.a, c.b); got != c.same {
			t.Errorf("%s: sameEndpoints = %v, want %v", c.name, got, c.same)
		}
	}
}

// A duplicate is not the same as a distinct second address. Counting rather than
// set-membership is what keeps ["a","a"] and ["a","b"] apart.
func TestSameEndpointsCountsDuplicates(t *testing.T) {
	if sameEndpoints([]string{"a", "a"}, []string{"a", "b"}) {
		t.Error("lists with different contents compared equal")
	}
}
