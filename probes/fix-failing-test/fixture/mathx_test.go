package mathx

import "testing"

func TestMax(t *testing.T) {
	if got := Max(3, 7); got != 7 {
		t.Fatalf("Max(3, 7) = %d, want 7", got)
	}
	if got := Max(9, 2); got != 9 {
		t.Fatalf("Max(9, 2) = %d, want 9", got)
	}
}
