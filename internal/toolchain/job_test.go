package toolchain

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
)

// A client polls by absolute index. Once the ring trims, naive indexing either
// re-sends lines the client already has or skips lines it never saw — both
// silent, and both look like a build that behaved oddly.
func TestLogFromSurvivesTrimming(t *testing.T) {
	j := &Job{}
	for i := range maxLogLines + 500 {
		j.appendLine(fmt.Sprintf("line %d", i))
	}

	lines, total := j.LogFrom(0)
	if total != maxLogLines+500 {
		t.Errorf("total = %d, want %d (every line ever, not the retained count)", total, maxLogLines+500)
	}
	if len(lines) != maxLogLines {
		t.Errorf("retained %d lines, want the %d-line cap", len(lines), maxLogLines)
	}
	// Asking from 0 after trimming must return the OLDEST RETAINED line, not
	// line 0, and not a slice offset by the wrong base.
	if lines[0] != "line 500" {
		t.Errorf("first retained line = %q, want %q", lines[0], "line 500")
	}
	if lines[len(lines)-1] != fmt.Sprintf("line %d", maxLogLines+499) {
		t.Errorf("last line = %q", lines[len(lines)-1])
	}

	// A client that has seen everything gets nothing back, not a duplicate tail.
	rest, _ := j.LogFrom(total)
	if len(rest) != 0 {
		t.Errorf("LogFrom(total) returned %d lines, want 0", len(rest))
	}

	// Mid-window request lands on the right line.
	mid, _ := j.LogFrom(1000)
	if len(mid) == 0 || mid[0] != "line 1000" {
		t.Errorf("LogFrom(1000) starts at %q, want %q", mid[0], "line 1000")
	}
}

func TestLogFromBeyondEndIsEmptyNotPanic(t *testing.T) {
	j := &Job{}
	j.appendLine("only")
	lines, total := j.LogFrom(99)
	if len(lines) != 0 || total != 1 {
		t.Errorf("LogFrom past the end = %v, %d", lines, total)
	}
}

func builderWith(answers map[Verb]any) (*Builder, *fakeRunner) {
	f := &fakeRunner{answers: answers}
	reg := testRegistry(cfgWith(map[string]config.ToolHost{"box1": {}}), f)
	return &Builder{Reg: reg}, f
}

// One slot, and the refusal must say what is holding it. "Busy" alone is the
// most annoying possible answer when the occupant is a 20-minute compile.
func TestSecondBuildIsRefusedAndNamesTheFirst(t *testing.T) {
	release := make(chan struct{})
	f := &fakeRunner{answers: map[Verb]any{
		VerbPreflight: Preflight{OK: true},
		VerbBuild:     Build{OK: true},
	}, block: release}
	reg := testRegistry(cfgWith(map[string]config.ToolHost{"box1": {}}), f)
	b := &Builder{Reg: reg}

	if _, err := b.Start("llama.cpp", "box1", false); err != nil {
		t.Fatalf("first start: %v", err)
	}
	_, err := b.Start("llama.cpp", "box1", false)
	if err == nil {
		t.Fatal("second build was allowed to start")
	}
	if !strings.Contains(err.Error(), "llama.cpp") || !strings.Contains(err.Error(), "box1") {
		t.Errorf("refusal does not name what is running: %v", err)
	}
	close(release)

	waitIdle(t, b)
	// The slot frees once it finishes.
	if _, err := b.Start("llama.cpp", "box1", false); err != nil {
		t.Errorf("slot not released after the first build ended: %v", err)
	}
}

func TestFinishedJobIsRetainedAsLast(t *testing.T) {
	b, _ := builderWith(map[Verb]any{
		VerbPreflight: Preflight{OK: true},
		VerbBuild:     Build{OK: true, Version: "10478", Stamp: "head=abc"},
	})
	j, err := b.Start("llama.cpp", "box1", false)
	if err != nil {
		t.Fatal(err)
	}
	waitIdle(t, b)

	cur, last := b.State()
	if cur != nil {
		t.Error("a finished build is still reported as current")
	}
	if last == nil {
		t.Fatal("finished build was not retained — 'did that work?' is asked minutes later")
	}
	if last.Status != "ok" || last.Version != "10478" {
		t.Errorf("last = %+v", last.Snapshot())
	}
	if last.FinishedAt.IsZero() || last.Elapsed() <= 0 {
		t.Error("timing not recorded")
	}
	if b.Find(j.ID) == nil {
		t.Error("Find cannot retrieve the finished job by id")
	}
}

// A failure has to be retained just as carefully — it is the case somebody
// actually needs the log for.
func TestFailedBuildRecordsTheReason(t *testing.T) {
	b, _ := builderWith(map[Verb]any{
		VerbPreflight: Preflight{OK: false, Missing: []string{"ffmpeg dev libs"}},
	})
	if _, err := b.Start("llama.cpp", "box1", false); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, b)

	_, last := b.State()
	if last == nil || last.Status != "failed" {
		t.Fatalf("expected a failed job, got %+v", last)
	}
	if !strings.Contains(last.Error, "ffmpeg") {
		t.Errorf("failure does not carry the reason: %q", last.Error)
	}
}

func TestStartWithoutRegistryIsRefused(t *testing.T) {
	b := &Builder{}
	if _, err := b.Start("llama.cpp", "box1", false); err == nil {
		t.Error("started a build with no registry")
	}
}

func waitIdle(t *testing.T, b *Builder) {
	t.Helper()
	for range 200 {
		if cur, _ := b.State(); cur == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("build did not finish")
}
