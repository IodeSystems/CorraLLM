package store

import (
	"context"
	"testing"
)

func TestAuthUserState(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	if nb, err := st.AuthNotBefore(ctx, "a@b.com"); err != nil || nb != 0 {
		t.Fatalf("unknown user: %d %v", nb, err)
	}
	if err := st.AuthRecordSignIn(ctx, "A@B.com", "10.0.0.1", 100); err != nil {
		t.Fatal(err)
	}
	if err := st.AuthRevoke(ctx, "a@b.com", 200); err != nil {
		t.Fatal(err)
	}
	// A later sign-in must not clear the revocation, nor a revocation the sign-in.
	if err := st.AuthRecordSignIn(ctx, "a@b.com", "10.0.0.2", 300); err != nil {
		t.Fatal(err)
	}
	if nb, _ := st.AuthNotBefore(ctx, " a@b.com"); nb != 200 {
		t.Errorf("not before = %d", nb)
	}
	all, err := st.AuthUserStates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := all["a@b.com"]; got != (AuthUserState{NotBeforeMS: 200, LastSignInMS: 300, LastSignInIP: "10.0.0.2"}) {
		t.Errorf("state = %+v", got)
	}
}
