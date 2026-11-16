package proxy

import (
	"net/http"
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
)

func modelsRig(t *testing.T) http.Handler {
	t.Helper()
	up := newGLMUpstream(t)
	m := modelTo(t, up.srv.URL, "chat")
	m.MaxConcurrent = 3
	m.Aliases = []string{"short", "legacy"}
	plain := modelTo(t, up.srv.URL, "chat")
	h, p := capRig(t, capStore(t), map[string]config.Model{"swift": m, "plain": plain})
	p.config().Lanes = map[string]config.Lane{"chat": {Members: []config.LaneMember{{Model: "swift"}}}}
	return h
}

// /v1/models lists aliases on the model entry only,
// lists aliases on the model entry only, and adds no per-alias entry.
func TestModelsSlotsAndAliases(t *testing.T) {
	h := modelsRig(t)
	var out struct {
		Data []struct {
			ID      string   `json:"id"`
			Slots   int      `json:"slots"`
			Aliases []string `json:"aliases"`
		} `json:"data"`
	}
	if c := getJSON(t, h, "/v1/models", &out); c != 200 {
		t.Fatalf("status %d", c)
	}
	byID := map[string]int{}
	for i, e := range out.Data {
		byID[e.ID] = i
	}
	if _, ok := byID["short"]; ok {
		t.Error("alias listed as its own entry")
	}
	sw := out.Data[byID["swift"]]
	if sw.Slots != 3 {
		t.Errorf("swift slots = %d, want config maxConcurrent 3", sw.Slots)
	}
	if len(sw.Aliases) != 2 || sw.Aliases[0] != "short" {
		t.Errorf("swift aliases = %v", sw.Aliases)
	}
	if a := out.Data[byID["plain"]].Aliases; a != nil {
		t.Errorf("plain aliases = %v, want omitted", a)
	}
}

func TestModelByIDResolvesAlias(t *testing.T) {
	h := modelsRig(t)
	for _, id := range []string{"swift", "short", "legacy"} {
		var e struct {
			ID    string `json:"id"`
			Slots int    `json:"slots"`
		}
		if c := getJSON(t, h, "/v1/models/"+id, &e); c != 200 {
			t.Fatalf("%s: status %d", id, c)
		}
		if e.ID != "swift" || e.Slots != 3 {
			t.Errorf("%s -> %+v, want swift/3", id, e)
		}
	}
	var lane struct {
		ID, Kind string
	}
	if c := getJSON(t, h, "/v1/models/chat", &lane); c != 200 || lane.ID != "chat" {
		t.Errorf("lane: %d %+v", c, lane)
	}
	if c := getJSON(t, h, "/v1/models/nope", nil); c != http.StatusNotFound {
		t.Errorf("unknown: status %d, want 404", c)
	}
}
