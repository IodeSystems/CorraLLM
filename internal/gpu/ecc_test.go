package gpu

import (
	"errors"
	"testing"
)

// Each value nvidia-smi gives for ecc.mode.current: only "Enabled" is ECC.
// Consumer cards (this box's 3080 and 5090) report "[N/A]".
func TestParseECCCSV(t *testing.T) {
	out := []byte(uuid3080 + ", [N/A]\n" +
		uuid5090 + ", Enabled\n" +
		"GPU-aaaa, Disabled\n" +
		"GPU-bbbb, \n")
	got, err := parseECCCSV(out)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{uuid3080: false, uuid5090: true, "GPU-aaaa": false, "GPU-bbbb": false}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if g, ok := got[k]; !ok || g != v {
			t.Errorf("%s: got %v (present %v), want %v", k, g, ok, v)
		}
	}
	if m, err := parseECCCSV([]byte("")); err != nil || len(m) != 0 {
		t.Errorf("empty output: %v %v", m, err)
	}
	if _, err := parseECCCSV([]byte("no comma here")); err == nil {
		t.Error("a row without a mode must be an error")
	}
}

// A failed read is not cached; the first success is.
func TestECCCachesSuccessOnly(t *testing.T) {
	eccCache.Lock()
	eccCache.m = nil
	eccCache.Unlock()
	t.Cleanup(func() {
		eccCache.Lock()
		eccCache.m = nil
		eccCache.Unlock()
	})
	calls := 0
	fail := true
	withRunCmd(t, func(string, ...string) ([]byte, error) {
		calls++
		if fail {
			return nil, errors.New("no nvidia-smi")
		}
		return []byte(uuid5090 + ", Enabled\n"), nil
	})
	if _, err := ECC(); err == nil {
		t.Fatal("want the probe error")
	}
	fail = false
	for range 2 {
		m, err := ECC()
		if err != nil || !m[uuid5090] {
			t.Fatalf("ECC() = %v, %v", m, err)
		}
	}
	if calls != 2 {
		t.Errorf("nvidia-smi called %d times, want 2 (one failure, one cached success)", calls)
	}
}
