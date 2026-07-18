package judge

import (
	"bufio"
	"encoding/json"
	"os"
)

// maxEntryBytes caps each persisted transcript entry's content (per P1 spec).
const maxEntryBytes = 2 << 10 // 2 KiB

// TranscriptEntry is one persisted agent conversation record. The runner dumps
// these per run (out/<ts>/transcripts/<combo>.jsonl); the judge reads them.
type TranscriptEntry struct {
	Kind       string `json:"kind"`
	ToolName   string `json:"toolName,omitempty"`
	ToolCallID string `json:"toolCallId,omitempty"`
	Content    string `json:"content"`
	CreatedAt  int64  `json:"createdAt"`
}

// NewTranscriptEntry builds an entry with content truncated to maxEntryBytes.
func NewTranscriptEntry(kind, toolName, toolCallID, content string, createdAt int64) TranscriptEntry {
	return TranscriptEntry{
		Kind:       kind,
		ToolName:   toolName,
		ToolCallID: toolCallID,
		Content:    truncBytes(content, maxEntryBytes),
		CreatedAt:  createdAt,
	}
}

// WriteTranscript writes entries as JSONL to path.
func WriteTranscript(path string, entries []TranscriptEntry) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, e := range entries {
		if err := enc.Encode(e); err != nil {
			return err
		}
	}
	return nil
}

// ReadTranscript parses a JSONL transcript file. A missing file returns
// (nil, false, nil) so the judge can degrade to the journal fallback.
func ReadTranscript(path string) (entries []TranscriptEntry, ok bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var e TranscriptEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return nil, false, err
		}
		entries = append(entries, e)
	}
	return entries, true, sc.Err()
}

func truncBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…[truncated]"
}
