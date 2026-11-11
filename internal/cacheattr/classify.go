package cacheattr

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

// Classify gives one turn its class. See classify's implementation for the
// rules; first match wins.
func Classify(in Input) Verdict {
	return Verdict{Class: ClassUnknown, CkptDist: -1}
}
