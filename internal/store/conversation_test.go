package store

import (
	"context"
	"fmt"
	"testing"
)

// TestCtxStats covers the two readings over interleaved conversations: every
// served turn counts per request, each conversation once at its last turn, a
// message count that drops starts a new session, and unserved turns count in
// neither.
func TestCtxStats(t *testing.T) {
	st, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	turn := func(ts int64, served, conv string, messages, prompt int) {
		t.Helper()
		if err := st.InsertActivity(Activity{TS: ts, Served: served, Path: "/v1/chat/completions", Status: 200,
			PromptTokens: prompt, Conversation: conv, ConversationMessages: messages}); err != nil {
			t.Fatal(err)
		}
	}
	turn(1, "m", "a", 2, 100)
	turn(2, "m", "b", 2, 50)
	turn(3, "m", "a", 4, 200)
	turn(4, "m", "b", 4, 70)
	turn(5, "m", "a", 6, 0)  // turned away: counts in neither reading
	turn(6, "m", "a", 2, 30) // restarted: "a" ended at 200, a new run begins
	turn(7, "n", "c", 2, 1000)
	turn(8, "m", "z", 2, 999) // outside the window

	rows, total, err := st.CtxStats(Between(1, 8))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Served != "m" || rows[1].Served != "n" {
		t.Fatalf("rows = %+v", rows)
	}
	m := rows[0]
	// Per request: 100, 50, 200, 70, 30.
	if m.Requests != 5 || m.RequestAvg != 90 || m.RequestP90 != 200 {
		t.Errorf("per request = %+v", m)
	}
	// Tails: a=200 (before the restart), b=70, a'=30.
	if m.Sessions != 3 || fmt.Sprintf("%.2f", m.TailAvg) != "100.00" || m.TailP90 != 200 {
		t.Errorf("tails = %+v", m)
	}
	if total.Requests != 6 || total.Sessions != 4 || total.RequestP90 != 1000 || total.TailP90 != 1000 {
		t.Errorf("total = %+v", total)
	}
}

// TestBackfillConversations derives rows from captured bodies, skips what does
// not parse, keeps a live row's key, and is a no-op when run again.
func TestBackfillConversations(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	body := func(first string, n int) string {
		msgs := `{"role":"system","content":"sys"},{"role":"user","content":"` + first + `"}`
		for i := 2; i < n; i++ {
			msgs += `,{"role":"assistant","content":"x"}`
		}
		return `{"model":"m","messages":[` + msgs + `]}`
	}
	for i, a := range []Activity{
		{TS: 1, Served: "m", Key: "k", Path: "/v1/chat/completions", PromptTokens: 10, ReqBody: body("one", 2)},
		{TS: 2, Served: "m", Key: "k", Path: "/v1/chat/completions", PromptTokens: 20, ReqBody: body("one", 4)},
		{TS: 3, Served: "m", Key: "k", Path: "/v1/chat/completions", PromptTokens: 5, ReqBody: `{"messages":[{"role":"us`},
		{TS: 4, Served: "m", Key: "k", Path: "/v1/embeddings", PromptTokens: 5, ReqBody: `{"input":"x"}`},
		{TS: 5, Served: "m", Key: "k", Path: "/v1/chat/completions", PromptTokens: 40, ReqBody: body("two", 2),
			Conversation: "live", ConversationMessages: 2},
	} {
		if err := st.InsertActivity(a); err != nil {
			t.Fatalf("row %d: %v", i, err)
		}
	}
	st.BackfillConversations(ctx)
	st.BackfillConversations(ctx)

	var n, live int
	if err := st.db.QueryRow(`SELECT COUNT(*), SUM(conversation = 'live') FROM activity_conversation`).Scan(&n, &live); err != nil {
		t.Fatal(err)
	}
	if n != 3 || live != 1 {
		t.Fatalf("rows = %d (live %d), want 3 with the live key kept", n, live)
	}
	rows, _, err := st.CtxStats(Window{})
	if err != nil {
		t.Fatal(err)
	}
	// "one" ran 2 → 4 messages: one session ending at 20; "live" is another.
	if len(rows) != 1 || rows[0].Requests != 3 || rows[0].Sessions != 2 || rows[0].TailP90 != 40 {
		t.Fatalf("stats = %+v", rows)
	}
}
