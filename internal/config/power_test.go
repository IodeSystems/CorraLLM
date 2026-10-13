package config

import (
	"strings"
	"testing"
)

func TestPowerValidate(t *testing.T) {
	servers := map[string]Server{
		"box1": {Devices: map[string]string{"gpu0": "GPU-ee90"}},
		"mac":  {},
	}
	for _, tc := range []struct {
		name string
		p    *Power
		want string
	}{
		{"unset", nil, ""},
		{"ok", &Power{Prometheus: "http://p:9090", Servers: map[string]ServerPower{"box1": {GPU: "g", Box: "b"}}}, ""},
		{"box only, no devices needed", &Power{Prometheus: "http://p:9090", Servers: map[string]ServerPower{"mac": {Box: "b"}}}, ""},
		{"no url", &Power{Servers: map[string]ServerPower{"box1": {GPU: "g"}}}, "prometheus URL"},
		{"unknown server", &Power{Prometheus: "http://p:9090", Servers: map[string]ServerPower{"box9": {Box: "b"}}}, "box9"},
		{"gpu without devices", &Power{Prometheus: "http://p:9090", Servers: map[string]ServerPower{"mac": {GPU: "g"}}}, "no devices"},
	} {
		err := tc.p.validate(servers)
		if (err == nil) != (tc.want == "") || (err != nil && !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: err %v, want %q", tc.name, err, tc.want)
		}
	}
}
