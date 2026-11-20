package store

import (
	"context"
	"math"
	"testing"
)

// TestKeyReportAggregatesByKeypServed verifies that KeyReport groups
// activity by (key, served) and sums every report field correctly.
func TestKeyReportAggregatesByKeyAndServed(t *testing.T) {
	st, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	rows := []Activity{
		// parent key, model A: 2 calls, 1 error
		{TS: 100, Served: "model-a", Key: "ck-parent", Status: 200,
			DwellMS: 100, QueuedMS: 10, PromptTokens: 100, CachedTokens: 50,
			CompletionTokens: 50, ReasoningTokens: 20, ReasoningMeasured: true, CostUSD: 0.01},
		{TS: 200, Served: "model-a", Key: "ck-parent", Status: 500,
			DwellMS: 200, QueuedMS: 20, PromptTokens: 200, CachedTokens: 100,
			CompletionTokens: 0, ReasoningTokens: -1, ReasoningMeasured: false, CostUSD: 0.02},

		// child key, model A: 1 call
		{TS: 300, Served: "model-a", Key: "ck-child-1", Status: 200,
			DwellMS: 300, QueuedMS: 30, PromptTokens: 300, CachedTokens: 150,
			CompletionTokens: 100, ReasoningTokens: 40, ReasoningMeasured: true, CostUSD: 0.03},

		// child key, model B: 2 calls
		{TS: 400, Served: "model-b", Key: "ck-child-1", Status: 200,
			DwellMS: 400, QueuedMS: 40, PromptTokens: 400, CachedTokens: 200,
			CompletionTokens: 200, ReasoningTokens: 60, ReasoningMeasured: true, CostUSD: 0.04},
		{TS: 500, Served: "model-b", Key: "ck-child-1", Status: 429,
			DwellMS: 50, QueuedMS: 50, PromptTokens: 0, CachedTokens: 0,
			CompletionTokens: 0, ReasoningTokens: -1, ReasoningMeasured: false, CostUSD: 0},
	}
	for _, a := range rows {
		if err := st.InsertActivity(a); err != nil {
			t.Fatal(err)
		}
	}

	keys := []string{"ck-parent", "ck-child-1"}
	got, err := st.KeyReport(Window{}, keys)
	if err != nil {
		t.Fatal(err)
	}
	// Expected groups: (ck-parent, model-a), (ck-child-1, model-a), (ck-child-1, model-b)
	if len(got) != 3 {
		t.Fatalf("want 3 groups, got %d: %+v", len(got), got)
	}

	// Find by (key, served).
	by := map[[2]string]KeyReport{}
	for _, r := range got {
		by[[2]string{r.Key, r.Served}] = r
	}

	// ck-parent / model-a: 2 calls, 1 error, sums of each field.
	r := by[[2]string{"ck-parent", "model-a"}]
	if r.Calls != 2 {
		t.Errorf("ck-parent/model-a calls = %d, want 2", r.Calls)
	}
	if r.Errors != 1 {
		t.Errorf("ck-parent/model-a errors = %d, want 1 (the 500)", r.Errors)
	}
	if r.DwellMS != 300 { // 100 + 200
		t.Errorf("ck-parent/model-a dwell = %d, want 300", r.DwellMS)
	}
	if r.QueuedMS != 30 { // 10 + 20
		t.Errorf("ck-parent/model-a queued = %d, want 30", r.QueuedMS)
	}
	if r.PromptTokens != 300 { // 100 + 200
		t.Errorf("ck-parent/model-a prompt = %d, want 300", r.PromptTokens)
	}
	if r.CachedTokens != 150 { // 50 + 100
		t.Errorf("ck-parent/model-a cached = %d, want 150", r.CachedTokens)
	}
	if r.CompletionTokens != 50 { // 50 + 0
		t.Errorf("ck-parent/model-a completion = %d, want 50", r.CompletionTokens)
	}
	if r.ReasoningTokens != 20 { // 20 (measured) + 0 (unmeasured, -1)
		t.Errorf("ck-parent/model-a reasoning = %d, want 20 (unmeasured row contributes 0)", r.ReasoningTokens)
	}
	if math.Abs(r.CostUSD-0.03) > 1e-9 { // 0.01 + 0.02
		t.Errorf("ck-parent/model-a cost = %v, want 0.03", r.CostUSD)
	}

	// ck-child-1 / model-a: 1 call, 0 errors.
	r = by[[2]string{"ck-child-1", "model-a"}]
	if r.Calls != 1 || r.Errors != 0 {
		t.Errorf("ck-child-1/model-a = %+v, want 1 call 0 errors", r)
	}
	if r.ReasoningTokens != 40 {
		t.Errorf("ck-child-1/model-a reasoning = %d, want 40", r.ReasoningTokens)
	}

	// ck-child-1 / model-b: 2 calls, 1 error (the 429).
	r = by[[2]string{"ck-child-1", "model-b"}]
	if r.Calls != 2 {
		t.Errorf("ck-child-1/model-b calls = %d, want 2", r.Calls)
	}
	if r.Errors != 1 {
		t.Errorf("ck-child-1/model-b errors = %d, want 1 (the 429)", r.Errors)
	}
	if r.DwellMS != 450 { // 400 + 50
		t.Errorf("ck-child-1/model-b dwell = %d, want 450", r.DwellMS)
	}
	if r.QueuedMS != 90 { // 40 + 50
		t.Errorf("ck-child-1/model-b queued = %d, want 90", r.QueuedMS)
	}
	// Unmeasured reasoning (-1) on the 429 row contributes 0.
	if r.ReasoningTokens != 60 { // 60 (measured) + 0 (unmeasured)
		t.Errorf("ck-child-1/model-b reasoning = %d, want 60", r.ReasoningTokens)
	}
}

// TestKeyReportWindowExcludesOutside verifies the window filter: rows
// outside [FromMS, ToMS) are not included.
func TestKeyReportWindowExcludesOutside(t *testing.T) {
	st, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	rows := []Activity{
		{TS: 100, Served: "m", Key: "k", Status: 200, DwellMS: 10},  // outside (before)
		{TS: 200, Served: "m", Key: "k", Status: 200, DwellMS: 20},  // inside
		{TS: 300, Served: "m", Key: "k", Status: 200, DwellMS: 30},  // inside
		{TS: 400, Served: "m", Key: "k", Status: 200, DwellMS: 40},  // outside (at upper bound, exclusive)
	}
	for _, a := range rows {
		if err := st.InsertActivity(a); err != nil {
			t.Fatal(err)
		}
	}

	// Window [150, 350): includes ts=200 and ts=300 only.
	got, err := st.KeyReport(Between(150, 350), []string{"k"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 group, got %d", len(got))
	}
	r := got[0]
	if r.Calls != 2 {
		t.Errorf("calls = %d, want 2 (ts 200 and 300)", r.Calls)
	}
	if r.DwellMS != 50 { // 20 + 30
		t.Errorf("dwell = %d, want 50", r.DwellMS)
	}
}

// TestKeyReportExcludesUnrequestedKeys verifies that only the keys in the
// filter are returned.
func TestKeyReportExcludesUnrequestedKeys(t *testing.T) {
	st, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	rows := []Activity{
		{TS: 100, Served: "m", Key: "ck-1", Status: 200, CostUSD: 0.1},
		{TS: 200, Served: "m", Key: "ck-2", Status: 200, CostUSD: 0.2},
		{TS: 300, Served: "m", Key: "ck-other", Status: 200, CostUSD: 0.9},
	}
	for _, a := range rows {
		if err := st.InsertActivity(a); err != nil {
			t.Fatal(err)
		}
	}

	// Request only ck-1 and ck-2; ck-other must not appear.
	got, err := st.KeyReport(Window{}, []string{"ck-1", "ck-2"})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range got {
		if r.Key == "ck-other" {
			t.Errorf("unrequested key ck-other appeared in results: %+v", r)
		}
	}
	if len(got) != 2 {
		t.Fatalf("want 2 groups, got %d: %+v", len(got), got)
	}
}

// TestKeyReportEmptyKeys returns nil for an empty key list.
func TestKeyReportEmptyKeys(t *testing.T) {
	st, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	if err := st.InsertActivity(Activity{TS: 100, Served: "m", Key: "k", Status: 200}); err != nil {
		t.Fatal(err)
	}

	got, err := st.KeyReport(Window{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("empty keys should return nil, got %v", got)
	}
}

// TestKeyReportNoRowsForKey returns empty when no activity exists for the
// requested keys.
func TestKeyReportNoRowsForKey(t *testing.T) {
	st, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	// Insert a row for a key that is NOT in the filter.
	if err := st.InsertActivity(Activity{TS: 100, Served: "m", Key: "other", Status: 200}); err != nil {
		t.Fatal(err)
	}

	got, err := st.KeyReport(Window{}, []string{"ck-unknown"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("want 0 groups for unknown key, got %d: %+v", len(got), got)
	}
}

// TestKeyReportReasoningUnmeasuredOnly verifies that when ALL rows have
// unmeasured reasoning (-1), the sum is 0, not negative.
func TestKeyReportReasoningUnmeasuredOnly(t *testing.T) {
	st, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	rows := []Activity{
		{TS: 100, Served: "m", Key: "k", Status: 200, CompletionTokens: 50, ReasoningTokens: -1, ReasoningMeasured: false},
		{TS: 200, Served: "m", Key: "k", Status: 200, CompletionTokens: 80, ReasoningTokens: -1, ReasoningMeasured: false},
	}
	for _, a := range rows {
		if err := st.InsertActivity(a); err != nil {
			t.Fatal(err)
		}
	}

	got, err := st.KeyReport(Window{}, []string{"k"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 group, got %d", len(got))
	}
	if got[0].ReasoningTokens != 0 {
		t.Errorf("all-unmeasured reasoning = %d, want 0 (not negative)", got[0].ReasoningTokens)
	}
}
