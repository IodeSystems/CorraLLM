package api

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/proc"
	"github.com/iodesystems/corrallm/internal/proxy"
	"github.com/iodesystems/corrallm/internal/sched"
	"github.com/iodesystems/corrallm/internal/store"
)

func requestHandlers(t *testing.T) *Handlers {
	t.Helper()
	cfg, err := config.Load(managedConfig(t, localProviderYAML))
	if err != nil {
		t.Fatal(err)
	}
	cfg.PriorityGroups = map[string]config.PriorityGroup{"default": {Weight: 1}}
	cfg.Keys = map[string]config.KeyPolicy{"sk-team": {Group: "default"}}
	h := storeBackedHandlers(t, cfg)
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mgr := proc.NewManager(&config.Config{})
	t.Cleanup(mgr.Shutdown)
	h.Store = st
	h.Proxy = proxy.New(cfg, mgr, sched.New(), st)
	h.Reload = func() error { h.Proxy.SetConfig(h.config()); return nil }
	return h
}

// Rules round-trip through the stored config; "default" removes the block.
func TestRequestRulesPersist(t *testing.T) {
	h := requestHandlers(t)
	in := &RequestRulesInput{}
	in.Body.Rules = []RequestRule{{Name: "lan", CIDRs: []string{" 192.168.1.0/24 "}}, {Name: "agents", Token: "${REQ_TOKEN}"}}
	if _, err := h.SetRequestRules(ctxAdmin, in); err != nil {
		t.Fatal(err)
	}
	got := reloadStored(t, h, "after rules").AccessRequests
	if got == nil || len(got.Rules) != 2 || got.Rules[0].CIDRs[0] != "192.168.1.0/24" || got.Rules[1].Token != "${REQ_TOKEN}" {
		t.Fatalf("stored rules = %+v", got)
	}
	bad := &RequestRulesInput{}
	bad.Body.Rules = []RequestRule{{Name: "empty"}}
	if _, err := h.SetRequestRules(ctxAdmin, bad); err == nil {
		t.Error("an empty rule was saved")
	}
	if _, err := h.SetRequestRules(ctxViewer, in); err == nil {
		t.Error("a viewer changed the rules")
	}
	def := &RequestRulesInput{}
	def.Body.UseDefault = true
	if _, err := h.SetRequestRules(ctxAdmin, def); err != nil {
		t.Fatal(err)
	}
	if reloadStored(t, h, "after default").AccessRequests != nil {
		t.Error("default left a block behind")
	}
}

// Approving a configured key's request for mint edits the stored config;
// managed keys list it with the value add.
func TestApproveRequestForConfiguredKey(t *testing.T) {
	h := requestHandlers(t)
	id := fileRequest(t, h, "sk-team", proxy.AccessAsk{Purpose: "projects", Mint: ptrTo(true)})
	in := &ApproveRequestInput{}
	in.Body.ID = id
	in.Body.Granted = KeyGrant{Group: "default", Mint: true, ValueAdds: []ValueAdd{{USD: 5, TTL: "2d"}}}
	if _, err := h.ApproveRequest(ctxAdmin, in); err != nil {
		t.Fatal(err)
	}
	if !reloadStored(t, h, "after approval").Keys["sk-team"].Mint {
		t.Error("mint not in the stored config")
	}
	if _, err := h.ApproveRequest(ctxAdmin, in); err == nil {
		t.Error("approved twice")
	}
	out, err := h.ListRequests(ctxAdmin, nil)
	if err != nil {
		t.Fatal(err)
	}
	var team *ManagedKey
	for i, k := range out.Body.Keys {
		if k.Key == "sk-team" {
			team = &out.Body.Keys[i]
		}
	}
	if team == nil || !team.Mint || len(team.ValueAdds) != 1 || team.ValueAdds[0].USD != 5 {
		t.Errorf("sk-team = %+v", team)
	}
	if len(out.Body.Requests) != 1 || out.Body.Requests[0].Status != "approved" || out.Body.Requests[0].DecidedBy != "ops@example.com" {
		t.Errorf("requests = %+v", out.Body.Requests)
	}
	if _, err := h.ListRequests(ctxViewer, nil); err == nil {
		t.Error("a viewer listed requests")
	}
}

func ptrTo[T any](v T) *T { return &v }

// fileRequest stores a pending request as POST /v1/access-requests would.
func fileRequest(t *testing.T, h *Handlers, key string, ask proxy.AccessAsk) string {
	t.Helper()
	b := mustJSON(t, ask)
	id := "ar-test" + time.Now().Format("150405.000000")
	if err := h.Store.SaveAccessRequest(store.AccessRequest{ID: id, ClaimHash: "x", Status: "pending",
		RequesterKey: key, Asked: b, CreatedMS: time.Now().UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	return id
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
