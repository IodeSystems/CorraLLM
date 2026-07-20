package run

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/corrallm/internal/bench/task"
)

type capturingRunner struct{ got *llm.ChatOpts }

func (c *capturingRunner) ChatStream(_ context.Context, _ []llm.Message, _ []llm.ToolDef, opts *llm.ChatOpts) (<-chan llm.StreamChunk, error) {
	c.got = opts
	ch := make(chan llm.StreamChunk)
	close(ch)
	return ch, nil
}

// A capability probe asks a yes/no question about the backend. Its answer must
// not depend on a sampler: bonsai runs at --temp 0.7 and failed cold / passed
// warm on identical input.
func TestMeteredRunner_PinsSamplingWhenSet(t *testing.T) {
	inner := &capturingRunner{}
	m := &meteredRunner{
		inner: inner, sc: &stageCounters{seen: map[string]int{}},
		temperature: &capabilityTemperature, seed: &capabilitySeed,
	}
	if _, err := m.ChatStream(context.Background(), nil, nil, &llm.ChatOpts{ToolChoice: "auto"}); err != nil {
		t.Fatal(err)
	}
	if inner.got == nil || inner.got.Temperature == nil || *inner.got.Temperature != 0 {
		t.Fatalf("temperature should be pinned to 0: %+v", inner.got)
	}
	if inner.got.Seed == nil || *inner.got.Seed != capabilitySeed {
		t.Errorf("seed should be pinned: %+v", inner.got)
	}
	// Unrelated fields must survive the override.
	if inner.got.ToolChoice != "auto" {
		t.Errorf("existing opts clobbered: %+v", inner.got)
	}
}

// Quality probes measure the model as it is actually served, sampler included.
func TestMeteredRunner_LeavesSamplingAloneByDefault(t *testing.T) {
	inner := &capturingRunner{}
	m := &meteredRunner{inner: inner, sc: &stageCounters{seen: map[string]int{}}}
	if _, err := m.ChatStream(context.Background(), nil, nil, &llm.ChatOpts{}); err != nil {
		t.Fatal(err)
	}
	if inner.got.Temperature != nil || inner.got.Seed != nil {
		t.Errorf("unpinned runner must not set sampling: %+v", inner.got)
	}
}

// The caller's opts are reused across turns by the agent loop; mutating them
// would leak the override into calls this runner does not own.
func TestMeteredRunner_DoesNotMutateCallerOpts(t *testing.T) {
	inner := &capturingRunner{}
	m := &meteredRunner{
		inner: inner, sc: &stageCounters{seen: map[string]int{}},
		temperature: &capabilityTemperature,
	}
	caller := &llm.ChatOpts{ToolChoice: "auto"}
	if _, err := m.ChatStream(context.Background(), nil, nil, caller); err != nil {
		t.Fatal(err)
	}
	if caller.Temperature != nil {
		t.Error("caller's opts were mutated")
	}
}

// A verdict from repeats, not from one sample.
func TestPublishCapabilityVerdict_AgreementRules(t *testing.T) {
	obs := func(v ...bool) []CapabilityObservation {
		out := make([]CapabilityObservation, 0, len(v))
		for _, b := range v {
			out = append(out, CapabilityObservation{Verified: b, Detail: "no image"})
		}
		return out
	}
	cases := []struct {
		name     string
		in       []CapabilityObservation
		verified bool
		flaky    bool
	}{
		// One sampled miss must not assert a capability is absent — this is the
		// bonsai false negative.
		{"one of two passed", obs(false, true), true, true},
		{"all passed", obs(true, true, true), true, false},
		// Every repeat agreeing is what a false verdict requires.
		{"none passed", obs(false, false, false), false, false},
		{"single pass", obs(true), true, false},
		{"single fail", obs(false), false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotVerified bool
			var gotDetail string
			srv, c := stubCapabilityClient(t, &gotVerified, &gotDetail)
			defer srv()
			publishCapabilityVerdict(context.Background(), c, "m", ModeWarm, capTask(), tc.in)
			if gotVerified != tc.verified {
				t.Errorf("verified: got %v want %v", gotVerified, tc.verified)
			}
			if flaky := containsFlaky(gotDetail); flaky != tc.flaky {
				t.Errorf("flaky marker: got %v want %v (detail %q)", flaky, tc.flaky, gotDetail)
			}
		})
	}
}

func containsFlaky(s string) bool {
	return len(s) >= 5 && s[:5] == "FLAKY"
}

func capTask() *task.Task {
	return &task.Task{
		Name:     "capability-vision",
		Class:    "capability",
		Requires: task.Requires{Modality: "image"},
	}
}

// stubCapabilityClient captures the verdict a publish would send.
func stubCapabilityClient(t *testing.T, verified *bool, detail *string) (func(), *residencyClient) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Verified bool   `json:"verified"`
			Detail   string `json:"detail"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		*verified, *detail = body.Verified, body.Detail
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	t.Setenv("TEST_ADMIN_TOKEN", "tok")
	c := newResidencyClient(cfgFor(srv.URL))
	if c == nil {
		t.Fatal("client should exist when a token is configured")
	}
	return srv.Close, c
}

// A capability probe must not be handed the workspace surface. Offering
// read_file/list_dir made capability-vision read "this image" as a file to find:
// it listed an empty directory and answered "I don't see any image files in the
// workspace", while the same prompt sent straight to the backend said "Red
// circles". maxToolCallsPerStage: 0 did not prevent it — the tools were still
// advertised.
func TestBuildSystemPrompt_CapabilityPromptMentionsNoWorkspace(t *testing.T) {
	got := buildSystemPrompt(&task.Task{Class: "capability", Name: "capability-vision"})
	for _, banned := range []string{"workspace", "read_file", "list_dir", "write_file"} {
		if strings.Contains(strings.ToLower(got), banned) {
			t.Errorf("capability prompt must not mention %q: %q", banned, got)
		}
	}
	if !strings.Contains(got, "already here") {
		t.Errorf("capability prompt should tell the model the content is attached: %q", got)
	}
}

// Quality probes keep the agent framing — they measure agentic behaviour.
func TestBuildSystemPrompt_QualityKeepsWorkspacePrompt(t *testing.T) {
	got := buildSystemPrompt(&task.Task{Class: "coding", Name: "edit-safety-rename"})
	if !strings.Contains(got, "workspace") {
		t.Errorf("coding prompt should keep the workspace framing: %q", got)
	}
}

// An explicit per-task System still wins over both defaults.
func TestBuildSystemPrompt_TaskOverrideWins(t *testing.T) {
	got := buildSystemPrompt(&task.Task{Class: "capability", System: "custom prompt"})
	if got != "custom prompt" {
		t.Errorf("task System should win: %q", got)
	}
}
