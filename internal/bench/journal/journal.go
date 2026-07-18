// Package journal is the append-only JSONL record of every llm-bench-mcp tool
// call. It is the source of truth for tool-usage checks (tool_called,
// tool_not_called, no_repeat_calls). llm-bench-mcp writes it; internal/check
// reads it.
package journal

import (
	"bufio"
	"encoding/json"
	"os"
	"sync"
)

// Entry is one journaled tool call.
type Entry struct {
	TS          int64           `json:"ts"`   // unix nanoseconds
	Tool        string          `json:"tool"` // tool name
	Args        json.RawMessage `json:"args"` // raw arguments object
	ResultBytes int             `json:"resultBytes"`
	Poisoned    bool            `json:"poisoned"`
	Bait        bool            `json:"bait"`
}

// ArgsString returns the args as a compact JSON string ("" when absent). Used
// for argContains matching and no_repeat_calls identity.
func (e Entry) ArgsString() string {
	if len(e.Args) == 0 {
		return ""
	}
	return string(e.Args)
}

// Writer appends entries to a JSONL file, safe for concurrent callers.
type Writer struct {
	mu sync.Mutex
	f  *os.File
}

// NewWriter opens path for append (creating it) and returns a Writer.
func NewWriter(path string) (*Writer, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &Writer{f: f}, nil
}

// Append writes one entry as a JSON line and flushes it to the OS so a reader
// polling the file sees it immediately.
func (w *Writer) Append(e Entry) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := w.f.Write(append(b, '\n')); err != nil {
		return err
	}
	return nil
}

// Close closes the underlying file.
func (w *Writer) Close() error { return w.f.Close() }

// Read parses a JSONL journal file into entries. A missing file yields an
// empty slice (a run that never called a tool).
func Read(path string) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, sc.Err()
}
