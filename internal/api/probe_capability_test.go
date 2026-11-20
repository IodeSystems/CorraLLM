package api

import (
	"context"
	"strings"
	"testing"
)

// A handler with no proxy refuses the probe with 503.
func TestProbeCapabilityNoProxy(t *testing.T) {
	h := &Handlers{}
	in := &ProbeCapabilityInput{}
	in.Name = "glm"
	_, err := h.ProbeCapability(context.Background(), in)
	if err == nil || !strings.Contains(err.Error(), "no proxy") {
		t.Fatalf("err = %v, want a 503 naming no proxy", err)
	}
}
