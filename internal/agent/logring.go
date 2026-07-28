package agent

import (
	"bytes"
	"sync"
)

// logRing is a bounded, sequence-numbered line buffer.
//
// Bounded because a backend can log for days and this is a supervisor, not a
// log store. Sequence-numbered because the primary needs to ask for "everything
// after what I already have" — see LogLine for why losing the first seconds of
// output costs a model its tuning profile.
//
// Sequence numbers are assigned per line and never reused, so `from` stays
// meaningful after old lines have been evicted: a caller asking for a sequence
// that has aged out is told where the buffer now starts rather than silently
// getting a gap.
type logRing struct {
	mu    sync.Mutex
	max   int
	lines []LogLine
	next  int64 // sequence of the NEXT line to be written
	part  []byte
}

func newLogRing(max int) *logRing {
	if max <= 0 {
		max = 500
	}
	return &logRing{max: max, next: 1}
}

// Write splits incoming output into lines. Partial trailing input is held until
// its newline arrives, so a line is never numbered twice.
func (r *logRing) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.part = append(r.part, p...)
	for {
		i := bytes.IndexByte(r.part, '\n')
		if i < 0 {
			break
		}
		line := string(bytes.TrimRight(r.part[:i], "\r"))
		r.part = r.part[i+1:]
		r.lines = append(r.lines, LogLine{Seq: r.next, Line: line})
		r.next++
		if len(r.lines) > r.max {
			r.lines = r.lines[len(r.lines)-r.max:]
		}
	}
	return len(p), nil
}

// Since returns every retained line with Seq >= from, plus the next sequence to
// ask for. A from of 0 means "everything retained".
func (r *logRing) Since(from int64) ([]LogLine, int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]LogLine, 0, len(r.lines))
	for _, l := range r.lines {
		if l.Seq >= from {
			out = append(out, l)
		}
	}
	return out, r.next
}

// LastSeq is the highest sequence written so far (0 if nothing).
func (r *logRing) LastSeq() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.next - 1
}
