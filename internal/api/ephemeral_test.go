package api

import (
	"context"
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/proc"
)

// A machine marked ephemeral that is not reporting in reads as away; one that
// is not marked keeps reading as a fault.
func TestOverviewEphemeralIsAway(t *testing.T) {
	cfg := &config.Config{
		Servers: map[string]config.Server{
			"laptop": {Agent: &config.AgentBinding{Endpoints: []string{"http://x"}}, Ephemeral: true},
			"rack":   {Agent: &config.AgentBinding{Endpoints: []string{"http://y"}}},
		},
		Models: map[string]config.Model{},
	}
	h := &Handlers{Cfg: cfg, Mgr: proc.NewManager(cfg)}
	out, err := h.Overview(context.Background(), &OverviewInput{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]ServerDef{}
	for _, s := range out.Body.Servers {
		got[s.Server] = s
	}
	if s := got["laptop"]; s.AgentStatus != "away" || !s.Ephemeral {
		t.Errorf("laptop = %q ephemeral=%v, want away", s.AgentStatus, s.Ephemeral)
	}
	if s := got["rack"]; s.AgentStatus != "unknown" {
		t.Errorf("rack = %q, want unknown (not marked, so still a fault)", s.AgentStatus)
	}
}

func TestSetServerEphemeralRefusesUnknown(t *testing.T) {
	h := &Handlers{Cfg: &config.Config{Servers: map[string]config.Server{}}}
	in := &SetServerEphemeralInput{Name: "nope"}
	in.Body.Ephemeral = true
	if _, err := h.SetServerEphemeral(context.Background(), in); err == nil {
		t.Error("marking an unknown server was accepted")
	}
}
