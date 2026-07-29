package stack

// IntStack is a LIFO stack of ints.
type IntStack struct {
	items []int
}

// Push adds v to the top of the stack.
func (s *IntStack) Push(v int) {
	s.items = append(s.items, v)
}

// Pop removes and returns the top element with ok=true, or 0, false when the
// stack is empty. It is currently unimplemented.
func (s *IntStack) Pop() (v int, ok bool) {
	panic("TODO: implement Pop")
}
