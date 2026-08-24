package sched

import (
	"context"
	"time"
)

// AGING: a caller that has been trying for a while outranks one that just
// arrived.
//
// Fairshare alone cannot see this. It compares a group's recent consumption
// against its weight, and a request that was rejected, backed off and returned
// looks exactly like a request making its first attempt — same group, same
// weight, no history. On a backend with one slot that is how a caller starves:
// every time it comes back, somebody else's fresh request is equally deserving
// and arrival order decides. The caller experiences a box that is up, answering
// other people, and permanently unavailable to them.
//
// What ages is the ATTEMPT, not the request object. The link between "the
// request I am admitting now" and "the request you rejected 40 seconds ago" is
// the ticket the proxy handed out on that rejection (see internal/proxy/ticket.go),
// which carries its own signed mint time. So the scheduler needs no memory of
// rejected requests: the caller brings its age with it.
//
// The credit is a DISCOUNT on the fairshare ratio rather than a separate
// priority tier. A tier would make age beat weight outright — one patient
// caller in a low-weight group would then stall a high-weight group
// indefinitely, which trades one starvation for another. A bounded discount
// lets age break ties and win close contests while leaving a large weight
// difference intact.

// agingUnit is the age at which a waiter counts as twice as deserving.
//
// 30 seconds is picked against the shape of the traffic this exists for: maxWait
// is 15s, so a caller that timed out and returned promptly is ~20-40s into its
// attempt on the retry that matters. Much shorter and every retry maxes out the
// credit immediately (age stops discriminating); much longer and the first
// retry gets no help at all, which is the case being fixed.
const agingUnit = 30 * time.Second

// maxAgingBoost caps the discount at 4x. Beyond it, age would start beating
// weight ratios large enough that the operator clearly meant them: a group
// weighted 5x another is a decision, and no amount of patience should silently
// undo it.
const maxAgingBoost = 4.0

type waitingSinceKey struct{}

// WithWaitingSince marks a request as the continuation of an attempt that began
// at `since`, so the scheduler can credit the time already spent trying.
//
// Carried on the context rather than through Admit's signature deliberately:
// Admit has sixty-odd call sites and this is optional per-request metadata that
// travels with the request anyway. A context with no mark behaves exactly as
// before, which is what every existing caller and test gets.
func WithWaitingSince(ctx context.Context, since time.Time) context.Context {
	if since.IsZero() {
		return ctx
	}
	return context.WithValue(ctx, waitingSinceKey{}, since)
}

// waitingSince reads the mark, or the zero time when there is none.
func waitingSince(ctx context.Context) time.Time {
	if ctx == nil {
		return time.Time{}
	}
	if t, ok := ctx.Value(waitingSinceKey{}).(time.Time); ok {
		return t
	}
	return time.Time{}
}

// agingBoost is how much more deserving an attempt started at `since` is than
// one starting now: 1 for a fresh request, growing to maxAgingBoost.
//
// Linear in age, not exponential: the point is a predictable "twice as
// deserving after 30 seconds", and an exponential curve would make the same
// wait mean wildly different things depending on where it started.
func agingBoost(now, since time.Time) float64 {
	if since.IsZero() {
		return 1
	}
	age := now.Sub(since)
	if age <= 0 {
		return 1
	}
	b := 1 + age.Seconds()/agingUnit.Seconds()
	if b > maxAgingBoost {
		return maxAgingBoost
	}
	return b
}

// WaitingSinceForTest exposes the mark to tests in other packages — the proxy
// owns one end of this contract and the scheduler the other, and the seam
// between them is exactly where a silent break would hide.
func WaitingSinceForTest(ctx context.Context) time.Time { return waitingSince(ctx) }
