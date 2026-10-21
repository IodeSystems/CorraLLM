package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// A marker turns into the final row in one transaction: one row, no marker.
func TestInflight_CompletionReplacesTheMarker(t *testing.T) {
	st, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	id, err := st.BeginInflight(Activity{TS: 1, Served: "m", Path: "/v1/chat/completions"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.InsertActivity(Activity{TS: 2, Served: "m", Path: "/v1/chat/completions", Status: 200,
		PromptTokens: 5, ReqBody: "req", InflightID: id}); err != nil {
		t.Fatal(err)
	}
	rows, _ := st.RecentActivity(Window{}, 10, "", "", "")
	if n, _ := st.InflightCount(); len(rows) != 1 || n != 0 || rows[0].Status != 200 {
		t.Fatalf("rows %+v, markers %d", rows, n)
	}
	// Nothing left to cut at the next start.
	if n, err := st.SweepInflight(); err != nil || n != 0 {
		t.Fatalf("sweep = %d, %v; want 0", n, err)
	}
}

// Restart in flight: a standing marker becomes one cut row, once, and a second
// sweep finds nothing. The row is a failure, not a 200 and not a caller hang-up.
func TestInflight_SweepRecordsCutRows(t *testing.T) {
	st, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if _, err := st.BeginInflight(Activity{TS: 100, Served: "m", Requested: "lane", Key: "k",
		Path: "/v1/chat/completions", Ticket: "t1", QueuedMS: 7}); err != nil {
		t.Fatal(err)
	}
	n, err := st.SweepInflight()
	if err != nil || n != 1 {
		t.Fatalf("sweep = %d, %v; want 1", n, err)
	}
	rows, _ := st.RecentActivity(Window{}, 10, "", "", "")
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
	r := rows[0]
	if r.Status != CutRestartStatus || r.Error != CutRestartError || r.Served != "m" || r.Requested != "lane" ||
		r.Key != "k" || r.Ticket != "t1" || r.TS != 100 || r.QueuedMS != 7 || r.PromptTokens != 0 {
		t.Errorf("cut row = %+v", r)
	}
	if n, _ := st.SweepInflight(); n != 0 {
		t.Errorf("second sweep cut %d rows", n)
	}
	if m, _ := st.InflightCount(); m != 0 {
		t.Errorf("%d markers left", m)
	}
}

// BenchmarkInflightMarker prices the marker on the single connection: the same
// completed-row insert with and without the BeginInflight that precedes it
// (the DELETE rides in the insert's transaction).
//
//	go test ./internal/store -run '^$' -bench InflightMarker -benchtime 20000x
func BenchmarkInflightMarker(b *testing.B) {
	for _, db := range []string{"memory", "file"} {
		for _, marker := range []bool{false, true} {
			name := db + "/plain"
			if marker {
				name = db + "/marker"
			}
			b.Run(name, func(b *testing.B) {
				path := ":memory:"
				if db == "file" {
					path = filepath.Join(b.TempDir(), "bench.db")
				}
				st, err := Open(context.Background(), path)
				if err != nil {
					b.Fatal(err)
				}
				defer func() { _ = st.Close() }()
				a := Activity{TS: 1, Served: "m", Requested: "m", Key: "k", Path: "/v1/chat/completions",
					Status: 200, PromptTokens: 100, CompletionTokens: 50, ReqBody: strings.Repeat("x", 4096),
					RespBody: strings.Repeat("y", 2048)}
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if marker {
						id, err := st.BeginInflight(a)
						if err != nil {
							b.Fatal(err)
						}
						a.InflightID = id
					}
					if err := st.InsertActivity(a); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
