package cacheattr

import "fmt"

// Input is everything the classifier reads about one turn and the previous
// turn of its conversation. -1 means not known throughout.
type Input struct {
	HaveStub     bool // the conversation was seen in the last 24 h
	HaveHashes   bool // ... and its previous turn's block hashes are in memory
	Reload       bool // the instance changed, or a backend start lies between the turns
	ReloadCause  string
	PromptTokens int
	CachedTokens int
	// PrevPromptTokens is the previous turn's prompt: the most a perfect cache
	// could reuse when the client only appended.
	PrevPromptTokens int
	MatchTokEst      int // tokens of the leading chars equal to the previous turn; -1 without hashes
	DivergeCause     string
	Slot, PrevSlot   int
	CtxCheckpoints   bool  // the backend runs with --ctx-checkpoints
	Boundaries       []int // earlier turns' boundaries, in tokens
	Evictions        int   // prompt-cache evictions between the turns; -1 without /metrics
	OtherTok         int64 // tokens the instance served to other conversations between the turns; -1 unknown
}

// Verdict is the classifier's answer.
type Verdict struct {
	Class   string
	Cause   string
	LostTok int
	// CkptDist is |cached − nearest boundary| in tokens; -1 when no boundary.
	CkptDist int
}

// Thresholds (P75 N4 defaults; owner decision D-thresholds may move them).
const (
	missFloorTok      = 1024    // a miss loses at least this many tokens ...
	missFloorPct      = 10      // ... and this % of the prompt
	divergeSlackTok   = 512     // the cache reused all that matched, within this ...
	divergeSlackPct   = 5       // ... or this % of the prompt
	ckptGapTok        = 1024    // checkpoint-limited: match − cached at least this
	ckptNearTok       = 256     // cached within this of a boundary ...
	ckptNearPct       = 1       // ... or this % of the prompt
	ckptZeroTok       = 256     // "re-processed from scratch": cached below this
	otherTrafficProxy = 300_000 // tokens of other traffic read as eviction without /metrics (N2's assumption)
)

// Classify gives one turn its class. First match wins:
//
//	cold                no stub: the conversation was not seen in 24 h
//	hit                 lost < max(1024 tok, 10% of prompt)
//	reload              the backend restarted between the turns
//	no-history          a stub without hashes (seen before a restart, or no body)
//	prefix-diverged     the cache reused all that still matched: the client changed the prompt
//	slot-switch         both slots known and different
//	checkpoint-limited  reuse stopped at an earlier boundary (hybrid model, --ctx-checkpoints)
//	evicted             evictions between the turns, or ≥300k tokens of other traffic without /metrics
//	unknown             none of the above
//
// lost is what a cache that kept the previous prompt would have saved:
// min(previous prompt, this prompt) − cached. DEVIATION from the N4 text,
// which measured lost against match_tok_est: with that, a miss
// (lost ≥ 1024) and prefix-diverged (match − cached < 512) exclude each
// other and the class could never fire. Against the previous prompt, a
// client-side divergence is a miss whose tokens the server could not have
// saved — which is what R5 needs to see.
func Classify(in Input) Verdict {
	v := Verdict{CkptDist: -1}
	if !in.HaveStub {
		v.Class = ClassCold
		return v
	}
	base := min(in.PrevPromptTokens, in.PromptTokens)
	v.LostTok = max(0, base-in.CachedTokens)
	for _, b := range in.Boundaries {
		if d := abs(in.CachedTokens - b); v.CkptDist < 0 || d < v.CkptDist {
			v.CkptDist = d
		}
	}
	if v.LostTok < max(missFloorTok, in.PromptTokens*missFloorPct/100) {
		v.Class = ClassHit
		return v
	}
	switch {
	case in.Reload:
		v.Class, v.Cause = ClassReload, in.ReloadCause
	case !in.HaveHashes || in.MatchTokEst < 0:
		v.Class = ClassNoHistory
	case in.MatchTokEst-in.CachedTokens < max(divergeSlackTok, in.PromptTokens*divergeSlackPct/100):
		v.Class, v.Cause = ClassDiverged, in.DivergeCause
	case in.Slot >= 0 && in.PrevSlot >= 0 && in.Slot != in.PrevSlot:
		v.Class, v.Cause = ClassSlotSwitch, fmt.Sprintf("slot %d→%d", in.PrevSlot, in.Slot)
	case in.CtxCheckpoints && in.MatchTokEst-in.CachedTokens >= ckptGapTok && checkpointBound(in, v.CkptDist):
		v.Class = ClassCheckpoint
		if in.CachedTokens < ckptZeroTok {
			v.Cause = "no checkpoint below the match: full re-process"
		} else {
			v.Cause = "reuse stopped at an earlier boundary"
		}
	case in.Evictions > 0:
		v.Class, v.Cause = ClassEvicted, fmt.Sprintf("%d evictions between turns", in.Evictions)
	case in.Evictions < 0 && in.OtherTok >= otherTrafficProxy:
		v.Class, v.Cause = ClassEvicted, fmt.Sprintf("%d tok of other traffic (no /metrics)", in.OtherTok)
	default:
		v.Class = ClassUnknown
	}
	return v
}

// checkpointBound: cached sits on an earlier boundary, or the cache started
// from nothing while every known boundary lies past the match (no checkpoint
// to fall back to — llama.cpp's "forcing full prompt re-processing").
func checkpointBound(in Input, dist int) bool {
	if dist >= 0 && dist <= max(ckptNearTok, in.PromptTokens*ckptNearPct/100) {
		return true
	}
	if in.CachedTokens >= ckptZeroTok || len(in.Boundaries) == 0 {
		return false
	}
	for _, b := range in.Boundaries {
		if b <= in.MatchTokEst {
			return false
		}
	}
	return true
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
