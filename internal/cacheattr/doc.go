// Package cacheattr says why a chat turn missed the backend's prompt cache
// (P75).
//
// A turn of a conversation re-sends everything before it, so a llama.cpp
// backend that kept the previous turn's KV needs to prefill only the new tail.
// When it prefills more, the tokens it re-read were lost somewhere: the
// backend restarted (reload), the client changed an earlier message
// (prefix-diverged), the turn went to the other slot (slot-switch), the
// hybrid model's reuse stopped at its last checkpoint (checkpoint-limited), or
// the entry was pushed out by other traffic (evicted). The Tracker keeps
// enough of each conversation's previous turn in memory to tell these apart,
// and the report turns a window of verdicts into recommendations — text only;
// nothing here changes a flag.
//
// NOTHING HERE MAY FAIL A REQUEST. The tracker runs after the reply has been
// written; every failure is logged and the turn is skipped.
package cacheattr

// Classes, in the order the classifier tries them (classify.go).
const (
	ClassHit        = "hit"
	ClassCold       = "cold"
	ClassReload     = "reload"
	ClassNoHistory  = "no-history"
	ClassDiverged   = "prefix-diverged"
	ClassSlotSwitch = "slot-switch"
	ClassCheckpoint = "checkpoint-limited"
	ClassEvicted    = "evicted"
	ClassUnknown    = "unknown"
)

// Classes lists every class, misses first in classifier order, for reports.
var Classes = []string{ClassReload, ClassNoHistory, ClassDiverged, ClassSlotSwitch,
	ClassCheckpoint, ClassEvicted, ClassUnknown, ClassCold, ClassHit}

// IsMiss reports whether a class is a miss (everything but hit and cold).
func IsMiss(class string) bool { return class != ClassHit && class != ClassCold && class != "" }
