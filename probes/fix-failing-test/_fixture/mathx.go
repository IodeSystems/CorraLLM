package mathx

// Max returns the larger of a and b.
func Max(a, b int) int {
	// BUG: this returns the SMALLER value.
	if a < b {
		return a
	}
	return b
}
