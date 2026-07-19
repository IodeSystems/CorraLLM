package calc

import (
	"errors"
	"strconv"
)

// ErrEmpty is returned by Len for an empty string.
var ErrEmpty = errors.New("empty")

// Parse converts s to an int. DEPRECATED — remove it.
func Parse(s string) (int, error) {
	return strconv.Atoi(s)
}

// Len returns the length of s, or ErrEmpty when s is empty.
func Len(s string) (int, error) {
	if s == "" {
		return 0, ErrEmpty
	}
	return len(s), nil
}
