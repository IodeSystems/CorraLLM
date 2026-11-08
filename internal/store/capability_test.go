package store

import (
	"reflect"
	"testing"
)

// A fact stored twice under one (endpoint, model, key) keeps the latest; facts
// of another key or model stay apart.
func TestCapabilityRoundTrip(t *testing.T) {
	st := openTest(t)
	a := CapabilityFact{Endpoint: "https://api.fireworks.ai/inference", Model: "glm", Key: "reasoning.efforts.refused",
		Value: `"none"`, Source: "refused", AtMS: 1000, Evidence: "sent reasoning_effort=none → 400"}
	b := CapabilityFact{Endpoint: a.Endpoint, Model: "glm", Key: "limits.output", Value: `"4096"`, Source: "observed", AtMS: 2000}
	for _, f := range []CapabilityFact{a, b} {
		if err := st.PutCapability(f); err != nil {
			t.Fatal(err)
		}
	}
	a.Value, a.AtMS = `"minimal,none"`, 3000
	if err := st.PutCapability(a); err != nil {
		t.Fatal(err)
	}
	got, err := st.Capabilities()
	if err != nil {
		t.Fatal(err)
	}
	if want := []CapabilityFact{b, a}; !reflect.DeepEqual(got, want) {
		t.Errorf("rows = %+v\nwant %+v", got, want)
	}
}
