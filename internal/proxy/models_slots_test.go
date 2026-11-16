package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
)

// /v1/models reports the capacity sched.Admit grants (config maxConcurrent),
// never a spawn-time tuned or banner count of a resident process.
func TestModelsSlotsFollowConfig(t *testing.T) {
	up := newGLMUpstream(t)
	m := modelTo(t, up.srv.URL, "chat")
	m.MaxConcurrent = 3
	h, _ := capRig(t, capStore(t), map[string]config.Model{"swift": m})
	var out struct {
		Data []struct {
			ID    string `json:"id"`
			Slots int    `json:"slots"`
		} `json:"data"`
	}
	if c := getJSON(t, h, "/v1/models", &out); c != 200 {
		t.Fatalf("status %d", c)
	}
	if len(out.Data) != 1 || out.Data[0].Slots != 3 {
		t.Errorf("got %+v, want swift slots 3", out.Data)
	}
}

func getJSON(t *testing.T, h http.Handler, path string, out any) int {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if out != nil && rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("%v: %s", err, rec.Body)
		}
	}
	return rec.Code
}
