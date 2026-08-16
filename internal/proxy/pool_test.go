package proxy

import (
	"strings"
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/freeroster"
)

// A catalogue with the shapes a free pool has to get right: free chat models,
// a free model that is NOT chat, a zero-priced router pseudo-model, and a
// perfectly good paid one.
func catalog() []freeroster.Entry {
	return []freeroster.Entry{
		{ID: "nvidia/nemotron-3-ultra-550b-a55b:free", Free: true, InputModality: "text", OutputModality: "text", ContextLength: 131072},
		{ID: "inclusionai/ling-3.0-flash:free", Free: true, InputModality: "text", OutputModality: "text", ContextLength: 65536},
		{ID: "google/gemma-4-26b-a4b-it:free", Free: true, InputModality: "text", OutputModality: "text", ContextLength: 32768},
		// Free and zero-priced, but it makes music — a chat lane must not get it.
		{ID: "google/lyria-2", Free: true, InputModality: "text", OutputModality: "audio", ContextLength: 32768},
		// A router pseudo-model.
		{ID: "openrouter/free", Free: true, InputModality: "text", OutputModality: "text", ContextLength: 0},
		// Perfectly good chat model, but paid.
		{ID: "anthropic/claude-opus-4", Free: false, InputModality: "text", OutputModality: "text", ContextLength: 200000},
	}
}

func from(source string, entries []freeroster.Entry) []poolCandidate {
	out := make([]poolCandidate, 0, len(entries))
	for _, e := range entries {
		out = append(out, poolCandidate{
			Target: config.VirtualTarget{Virtual: "free", Source: source, Credential: "default"},
			Entry:  e,
		})
	}
	return out
}

func ids(cands []poolCandidate) []string {
	var out []string
	for _, c := range cands {
		out = append(out, c.Target.Source+"/"+c.Entry.ID)
	}
	return out
}

var freeChat = &config.Virtual{
	Filter: config.DiscoverFilter{
		Free: true, InputModality: "text", OutputModality: "text",
		MinContext: 8192, Exclude: []string{"openrouter/free"},
	},
}

func TestPoolFiltersToFreeChatModels(t *testing.T) {
	got := selectPool(from("openrouter", catalog()), freeChat)
	want := []string{
		"openrouter/nvidia/nemotron-3-ultra-550b-a55b:free",
		"openrouter/inclusionai/ling-3.0-flash:free",
		"openrouter/google/gemma-4-26b-a4b-it:free",
	}
	if len(got) != len(want) {
		t.Fatalf("kept %v, want %v", ids(got), want)
	}
	for i, w := range want {
		if ids(got)[i] != w {
			t.Errorf("position %d: got %s, want %s", i, ids(got)[i], w)
		}
	}
	// The specific failures this filter exists to prevent.
	for _, bad := range ids(got) {
		if bad == "openrouter/google/lyria-2" {
			t.Error("a music model was admitted to a chat pool on the strength of costing nothing")
		}
		if bad == "openrouter/openrouter/free" {
			t.Error("a router pseudo-model was admitted")
		}
		if bad == "openrouter/anthropic/claude-opus-4" {
			t.Error("a PAID model entered the free pool")
		}
	}
}

// TestPoolSpansProviders is the whole reason this is a pool and not a
// per-provider filter: one catalogue assembled from several.
func TestPoolSpansProviders(t *testing.T) {
	cands := append(
		from("openrouter", catalog()),
		from("deepinfra", []freeroster.Entry{
			{ID: "meta/llama-4-free", Free: true, InputModality: "text", OutputModality: "text", ContextLength: 128000},
		})...,
	)
	got := ids(selectPool(cands, freeChat))
	if len(got) != 4 {
		t.Fatalf("pool = %v, want both providers' free models", got)
	}
	var sawDeepinfra bool
	for _, g := range got {
		if g == "deepinfra/meta/llama-4-free" {
			sawDeepinfra = true
		}
	}
	if !sawDeepinfra {
		t.Errorf("the second provider contributed nothing: %v", got)
	}
}

// TestPoolCapIsFairAcrossMembers is the bug live data found. A single global
// sort by context length looks right and silently excludes any member that does
// not report one — DeepInfra reports neither pricing nor context_length, so
// every row is context 0 and lands last. Under a cap it contributed nothing at
// all: the very crowding-out the pool-wide cap existed to prevent.
func TestPoolCapIsFairAcrossMembers(t *testing.T) {
	unreported := []freeroster.Entry{
		{ID: "a/one", Free: true, InputModality: "text", OutputModality: "text"},
		{ID: "a/two", Free: true, InputModality: "text", OutputModality: "text"},
		{ID: "a/three", Free: true, InputModality: "text", OutputModality: "text"},
	}
	cands := append(from("openrouter", catalog()), from("deepinfra", unreported)...)
	spec := *freeChat
	spec.Filter.MinContext = 0 // the rows report no window; do not exclude on it
	spec.Limit = 4
	got := ids(selectPool(cands, &spec))
	if len(got) != 4 {
		t.Fatalf("cap not applied: %v", got)
	}
	perSource := map[string]int{}
	for _, g := range got {
		perSource[strings.SplitN(g, "/", 2)[0]]++
	}
	if perSource["deepinfra"] == 0 {
		t.Errorf("a member reporting no context length was shut out of a capped pool: %v", got)
	}
	if perSource["openrouter"] == 0 {
		t.Errorf("the other member was shut out: %v", got)
	}
	if perSource["deepinfra"] != 2 || perSource["openrouter"] != 2 {
		t.Errorf("cap not shared evenly: %v (%v)", perSource, got)
	}
}

// TestPoolCapIsGlobal: the cap belongs to the pool. Applied per member, one
// verbose provider would crowd the others out — which is the failure mode that
// made per-provider filters the wrong shape for a free tier.
func TestPoolCapIsGlobal(t *testing.T) {
	cands := append(
		from("openrouter", catalog()),
		from("deepinfra", []freeroster.Entry{
			{ID: "meta/llama-4-free", Free: true, InputModality: "text", OutputModality: "text", ContextLength: 128000},
		})...,
	)
	spec := *freeChat
	spec.Limit = 2
	got := ids(selectPool(cands, &spec))
	if len(got) != 2 {
		t.Fatalf("cap not applied across the pool: %v", got)
	}
	// One from each member, each that member's largest window.
	want := []string{"deepinfra/meta/llama-4-free", "openrouter/nvidia/nemotron-3-ultra-550b-a55b:free"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("cap kept %v, want one from each member %v", got, want)
		}
	}
}

// TestPoolIsDeterministic: a lane assembled from a map must not reshuffle
// between refreshes, or routing stops being reproducible.
func TestPoolIsDeterministic(t *testing.T) {
	first := ids(selectPool(from("openrouter", catalog()), freeChat))
	for i := 0; i < 20; i++ {
		got := ids(selectPool(from("openrouter", catalog()), freeChat))
		for j := range first {
			if got[j] != first[j] {
				t.Fatalf("order changed between passes: %v then %v", first, got)
			}
		}
	}
}
