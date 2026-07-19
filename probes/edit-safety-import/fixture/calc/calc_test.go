package calc

import (
	"errors"
	"testing"
)

func TestLen(t *testing.T) {
	if n, err := Len("abc"); err != nil || n != 3 {
		t.Fatalf("Len(abc) = (%d, %v), want (3, nil)", n, err)
	}
	if _, err := Len(""); !errors.Is(err, ErrEmpty) {
		t.Fatalf("Len(\"\") err = %v, want ErrEmpty", err)
	}
}
