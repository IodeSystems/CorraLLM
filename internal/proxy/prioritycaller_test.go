package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/iodesystems/corrallm/internal/sched"
)

// TestAdmitCtxMarksPriorityCaller: the priority caller's requests are admitted
// under sched.WithPriority; every other key, and a proxy with no priority
// caller, is not.
func TestAdmitCtxMarksPriorityCaller(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	p := &Proxy{}
	if sched.PriorityForTest(p.admitCtx(context.Background(), r, "bench-1")) {
		t.Fatal("priority without a priority caller")
	}
	p.SetPriorityCaller(func(key string) bool { return key == "bench-1" })
	if !sched.PriorityForTest(p.admitCtx(context.Background(), r, "bench-1")) {
		t.Fatal("bench key not marked priority")
	}
	if sched.PriorityForTest(p.admitCtx(context.Background(), r, "sk-aw4")) {
		t.Fatal("other key marked priority")
	}
}
