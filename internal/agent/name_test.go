package agent

import "testing"

// A config key ends up in YAML, logs and URLs, so a hostname has to be reduced
// to something safe. The motivating case is a Mac: hostnames there are
// title-cased and carry a .local suffix.
func TestProposedServerName_SanitisesAHostname(t *testing.T) {
	for in, want := range map[string]string{
		"CarlsMacBookPro.local": "carlsmacbookpro",
		"box1":                  "box1",
		"my_host":               "my-host",
		"-weird-.domain.com":    "weird",
	} {
		if got := sanitiseHostname(in); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
	// Never empty: a server called "" would be created and then be
	// unaddressable.
	if got := sanitiseHostname("!!!"); got == "" {
		t.Error("an unusable hostname must fall back to a name, not empty")
	}
}
