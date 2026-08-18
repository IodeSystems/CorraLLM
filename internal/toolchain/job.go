package toolchain

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// Builder runs tool builds as a background job with ONE slot.
//
// One at a time, globally, and that is a decision rather than a simplification.
// A build saturates a machine — `cmake --build --parallel $(nproc)` takes every
// core — so two at once finish later than two in sequence and make each other's
// timings meaningless. It also makes the question the UI has to answer small:
// there is a build or there isn't, and the modal never has to explain which of
// several it is showing.
//
// The last finished job is kept after it ends, because "did that work?" is
// asked minutes later, usually by somebody who closed the modal. Losing the
// result the moment it completes would mean rerunning a 20-minute build to find
// out how the last one went.
type Builder struct {
	Reg *Registry

	mu      sync.Mutex
	current *Job
	last    *Job
	seq     int
}

// Job is one build, running or finished.
type Job struct {
	ID   string `json:"id"`
	Tool string `json:"tool"`
	Host string `json:"host"`
	// Status is "running", "ok" or "failed".
	Status     string    `json:"status"`
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt,omitempty"`
	// Skipped means the stamp already matched, so nothing was compiled. Worth
	// reporting rather than hiding: a build that "finished" in two seconds
	// should be explicable.
	Skipped bool   `json:"skipped"`
	Version string `json:"version,omitempty"`
	Stamp   string `json:"stamp,omitempty"`
	Error   string `json:"error,omitempty"`

	mu  sync.Mutex
	log []string
	// total counts every line EVER appended, which the retained ring does not.
	// A client polling by index needs absolute positions, or it silently
	// re-reads or skips lines once trimming starts.
	total int
}

// maxLogLines bounds a job's retained output.
//
// A CUDA build emits tens of thousands of lines, nearly all of them nvcc
// warnings about somebody else's headers. The tail is what diagnoses a failure
// — cmake's error is within a few lines of the end — so keeping the last couple
// of thousand costs little and loses nothing that was going to be read.
const maxLogLines = 2000

func (j *Job) appendLine(s string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.log = append(j.log, s)
	j.total++
	if len(j.log) > maxLogLines {
		j.log = j.log[len(j.log)-maxLogLines:]
	}
}

// LogFrom returns lines from index `from` onward, plus the total line count.
//
// Indexed rather than whole-log so the UI can poll for what it has not seen.
// The count is the total EVER appended, not the retained length, so a client
// that falls behind a trimmed ring is told to resync instead of silently
// receiving the wrong lines.
func (j *Job) LogFrom(from int) (lines []string, total int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	total = j.total
	if from < 0 {
		from = 0
	}
	// Where the retained window starts in absolute terms.
	base := j.total - len(j.log)
	if from < base {
		from = base
	}
	i := from - base
	if i > len(j.log) {
		i = len(j.log)
	}
	out := make([]string, len(j.log)-i)
	copy(out, j.log[i:])
	return out, total
}

// Snapshot copies a job without its log, safe to marshal.
func (j *Job) Snapshot() Job {
	j.mu.Lock()
	defer j.mu.Unlock()
	return Job{
		ID: j.ID, Tool: j.Tool, Host: j.Host, Status: j.Status,
		StartedAt: j.StartedAt, FinishedAt: j.FinishedAt,
		Skipped: j.Skipped, Version: j.Version, Stamp: j.Stamp, Error: j.Error,
	}
}

// Elapsed is how long the job ran, or has been running.
func (j *Job) Elapsed() time.Duration {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.FinishedAt.IsZero() {
		return time.Since(j.StartedAt)
	}
	return j.FinishedAt.Sub(j.StartedAt)
}

// Start begins a build, refusing if one is already running.
//
// The refusal names what is running, because "busy" without saying what is the
// most annoying possible answer when the thing running is a 20-minute compile
// somebody else started.
func (b *Builder) Start(tool, host string, force bool) (*Job, error) {
	if b.Reg == nil {
		return nil, fmt.Errorf("no toolchain registry configured")
	}

	b.mu.Lock()
	if b.current != nil {
		cur := b.current
		b.mu.Unlock()
		return nil, fmt.Errorf("a build is already running: %s on %s, started %s ago — one at a time, because a build takes every core on the box",
			cur.Tool, cur.Host, cur.Elapsed().Round(time.Second))
	}
	b.seq++
	j := &Job{
		ID:        fmt.Sprintf("b%d", b.seq),
		Tool:      tool,
		Host:      host,
		Status:    "running",
		StartedAt: time.Now(),
	}
	b.current = j
	b.mu.Unlock()

	go b.run(j, force)
	return j, nil
}

// run performs the build and files the result.
//
// context.Background, NOT the request's: the HTTP call that started this
// returns immediately, and tying a 20-minute compile to a connection that has
// already closed would cancel every build the moment its modal was dismissed.
func (b *Builder) run(j *Job, force bool) {
	ctx, cancel := context.WithTimeout(context.Background(), Timeout(VerbBuild))
	defer cancel()

	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(pr)
		// cmake and nvcc emit long lines; the default 64K token limit would
		// error out mid-build and silently truncate the log from there on.
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			j.appendLine(strings.TrimRight(sc.Text(), "\r"))
		}
	}()

	res, err := b.Reg.Build(ctx, j.Tool, j.Host, force, pw)
	_ = pw.Close()
	<-done

	j.mu.Lock()
	j.FinishedAt = time.Now()
	if err != nil {
		j.Status, j.Error = "failed", err.Error()
	} else {
		j.Status = "ok"
		if res != nil {
			j.Skipped, j.Version, j.Stamp = res.Skipped, res.Version, res.Stamp
		}
	}
	j.mu.Unlock()

	b.mu.Lock()
	b.last, b.current = j, nil
	b.mu.Unlock()
}

// State returns the running job (if any) and the last finished one.
func (b *Builder) State() (current, last *Job) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.current, b.last
}

// Find returns a job by id, current or last. Nil when neither matches — a job
// older than the last one is genuinely gone, and saying so beats returning the
// wrong one.
func (b *Builder) Find(id string) *Job {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.current != nil && b.current.ID == id {
		return b.current
	}
	if b.last != nil && b.last.ID == id {
		return b.last
	}
	return nil
}
