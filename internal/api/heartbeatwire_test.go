package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/iodesystems/corrallm/internal/agent"
	"github.com/iodesystems/corrallm/internal/config"
)

// The per-card fields are additive on the wire, through the REAL request
// validation: a heartbeat from an agent older than them (no `gpus`, no
// `memoryByDevice`) must still be accepted, as must a new agent's null split
// ("cannot attribute per device") and empty one ("nothing on any card").
func TestAgentHeartbeat_PerCardFieldsAreOptional(t *testing.T) {
	cfg, err := config.LoadBytesForTest([]byte(`
servers:
  box1:
    pools: { gpu0: 32607MiB }
    agent: { endpoints: ["http://127.0.0.1:6503"], token: tok }
`))
	if err != nil {
		t.Fatal(err)
	}
	live := agent.NewLiveness()
	r := chi.NewRouter()
	if _, err := BuildGateway(r, &Handlers{Cfg: cfg, Liveness: live}); err != nil {
		t.Fatal(err)
	}
	const hello = `"hello":{"protocol":1,"version":"dev","hostname":"h","os":"linux","arch":"amd64","booted":1}`
	const backend = `"id":"b1","key":"m","model":"m","cmd":"x","alive":true,"exited":false,"started":1,"memoryMiB":0,"lastSeq":0`
	for name, body := range map[string]string{
		"older agent": `{"server":"box1",` + hello + `,"backends":[{` + backend + `}],` +
			`"capacity":{"gpu":{"name":"g","totalBytes":1,"usedBytes":1,"freeBytes":0},"perProcess":true}}`,
		"null split": `{"server":"box1",` + hello + `,"backends":[{` + backend + `,"memoryByDevice":null}],` +
			`"capacity":{"perProcess":true,"gpus":[{"name":"g","totalBytes":1,"usedBytes":1,"freeBytes":0,"index":0,"uuid":"GPU-1"}]}}`,
		"empty split": `{"server":"box1",` + hello + `,"backends":[{` + backend + `,"memoryByDevice":{}}],"capacity":{"perProcess":true}}`,
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/heartbeat", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer tok")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	if c, ok := live.Capacity("box1"); !ok || c.PerProcess != true {
		t.Errorf("capacity not recorded: %+v %v", c, ok)
	}
}
