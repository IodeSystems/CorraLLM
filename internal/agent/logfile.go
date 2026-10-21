package agent

import (
	"bytes"
	"io"
	"os"
	"time"
)

// A backend writes its output to a FILE, and the agent tails that file.
//
// It used to be a pipe into the agent. That made every backend depend on the
// agent staying alive: with the agent gone, the backend's next write to the
// pipe gets SIGPIPE/EPIPE, and llama-server dies of it — so a backend that was
// meant to survive an agent restart died on its first log line afterwards. A
// file has no reader to lose. The child holds its own O_APPEND descriptor, and
// any agent, the one that started it or the next one, can read along.

// logCapBytes bounds one backend's log file. A backend can log for weeks.
//
// The file cannot be rotated by renaming: the child keeps writing to the inode
// it opened, so the renamed file would keep growing and the new name would stay
// empty. Instead the tail is copied aside (<log>.1) and the file is truncated in
// place; the child's O_APPEND writes then land at the new end. The few bytes
// written between the copy and the truncate are lost, which is the price of not
// being able to make the child reopen its output.
const logCapBytes = 32 << 20

// logSeedBytes is how much of an existing file a NEW tailer replays, so an
// adopted backend's recent output is in the ring right away (the primary reads
// from sequence 0 after a restart, and llama.cpp's banner is useful there).
const logSeedBytes = 64 << 10

// logPoll is how often the tailer looks for new output. The primary polls the
// ring every 500ms, so faster than that only burns syscalls.
const logPoll = 200 * time.Millisecond

// openBackendLog creates the file a backend writes to. O_APPEND is load-bearing:
// it is what makes the child's writes follow a truncation instead of leaving a
// sparse hole the size of the old file.
func openBackendLog(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND|os.O_TRUNC, 0o600)
}

// tailLog copies path into w until done closes, then drains what is left and
// returns. seed replays the last logSeedBytes of an existing file first.
func tailLog(path string, w io.Writer, seed bool, done <-chan struct{}, capBytes int64) {
	var off int64
	if seed {
		if st, err := os.Stat(path); err == nil && st.Size() > logSeedBytes {
			off = st.Size() - logSeedBytes
		}
	}
	skipPartial := off > 0 // starting mid-file: drop the torn first line
	read := func() {
		f, err := os.Open(path)
		if err != nil {
			return
		}
		defer func() { _ = f.Close() }()
		st, err := f.Stat()
		if err != nil {
			return
		}
		if st.Size() < off {
			off = 0 // truncated by the cap: start again from the top
		}
		if st.Size() > capBytes {
			rotateLog(path, f, st.Size(), capBytes)
			off = 0
			return
		}
		if st.Size() == off {
			return
		}
		buf := make([]byte, st.Size()-off)
		n, _ := f.ReadAt(buf, off)
		buf = buf[:n]
		// Hold back a trailing partial line until its newline arrives: the
		// ring numbers lines, and a line split across two reads would be
		// numbered twice.
		end := bytes.LastIndexByte(buf, '\n')
		if end < 0 {
			return
		}
		chunk := buf[:end+1]
		off += int64(end + 1)
		if skipPartial {
			skipPartial = false
			if i := bytes.IndexByte(chunk, '\n'); i >= 0 {
				chunk = chunk[i+1:]
			}
		}
		_, _ = w.Write(chunk)
	}
	t := time.NewTicker(logPoll)
	defer t.Stop()
	for {
		select {
		case <-done:
			read()
			// A final unterminated line is often the one that says why it died.
			if f, err := os.Open(path); err == nil {
				if rest, err := io.ReadAll(io.NewSectionReader(f, off, 1<<20)); err == nil && len(rest) > 0 {
					_, _ = w.Write(append(rest, '\n'))
				}
				_ = f.Close()
			}
			return
		case <-t.C:
			read()
		}
	}
}

// rotateLog keeps the newest quarter of the cap as <path>.1 and empties path.
func rotateLog(path string, f *os.File, size, capBytes int64) {
	keep := capBytes / 4
	if keep > size {
		keep = size
	}
	buf := make([]byte, keep)
	n, _ := f.ReadAt(buf, size-keep)
	_ = os.WriteFile(path+".1", buf[:n], 0o600)
	_ = os.Truncate(path, 0)
}
