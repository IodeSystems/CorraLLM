package handoff

import (
	"encoding/json"
	"strings"
	"testing"
)

// LocalServers must tell "none" (empty) from "not sent" (an older
// predecessor, nil): the successor holds every agent spawn for the second.
func TestStatus_LocalServersNoneVersusAbsent(t *testing.T) {
	b, err := json.Marshal(Status{Draining: true, LocalServers: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"localServers":[]`) {
		t.Errorf("empty list encoded as %s", b)
	}
	var none Status
	if err := json.Unmarshal(b, &none); err != nil || none.LocalServers == nil {
		t.Errorf("empty list decoded as nil (%v)", err)
	}
	var older Status
	if err := json.Unmarshal([]byte(`{"draining":true,"local":2}`), &older); err != nil || older.LocalServers != nil {
		t.Errorf("absent field decoded as %#v (%v), want nil", older.LocalServers, err)
	}
}
