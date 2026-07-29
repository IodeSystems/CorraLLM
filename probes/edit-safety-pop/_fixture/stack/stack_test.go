package stack

import "testing"

func TestPushPop(t *testing.T) {
	var s IntStack
	s.Push(1)
	s.Push(2)
	s.Push(3)
	for _, want := range []int{3, 2, 1} {
		got, ok := s.Pop()
		if !ok || got != want {
			t.Fatalf("Pop() = (%d, %v), want (%d, true)", got, ok, want)
		}
	}
	if got, ok := s.Pop(); ok || got != 0 {
		t.Fatalf("Pop() on empty = (%d, %v), want (0, false)", got, ok)
	}
}
