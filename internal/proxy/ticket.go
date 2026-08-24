package proxy

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/iodesystems/corrallm/internal/sched"
)

// A TICKET is the identity of one caller's attempt to get served, across every
// rejection it takes on the way.
//
// Without it a 429 is anonymous. The caller comes back, and the box has no way
// to know this is the same customer returning for the fourth time rather than a
// new arrival — so the activity log correlates a return to its rejection by
// "next request from the same key", a heuristic that is wrong whenever a caller
// has two requests in flight, and the scheduler cannot tell a patient caller
// from a fresh one at all.
//
// The protocol is deliberately one-directional and optional:
//
//   - a caller MAY send X-Corrallm-Request-Id (or X-Request-Id) with its own id;
//   - if it does not, and we reject it, WE mint one and hand it back on the 429
//     (X-Corrallm-Ticket, and `error.ticket` in the body);
//   - a caller that echoes it on the retry gets its attempts linked. One that
//     ignores it is served exactly as before.
//
// Nothing depends on the client cooperating. A client that never echoes simply
// gets a fresh ticket per attempt, which is the behaviour that existed before
// tickets did.
const (
	// HeaderRequestID is what a caller sends when it already has an id of its
	// own. Accepted from either spelling — X-Request-Id is the convention
	// everything else in the ecosystem uses, and refusing it because we prefer
	// our own prefix would make the common case the one that does not work.
	HeaderRequestID    = "X-Corrallm-Request-Id"
	HeaderRequestIDAlt = "X-Request-Id"
	// HeaderTicket is what we hand BACK on a rejection.
	HeaderTicket = "X-Corrallm-Ticket"
)

// ticketPrefix marks an id this daemon minted. A caller-supplied id keeps
// whatever shape the caller chose; only ours is parsed for an age.
const ticketPrefix = "tkt_"

// maxTicketLen bounds what we will echo back and store. A caller id is data
// from the wire: unbounded, it lands in the activity log and every UI that
// renders it.
const maxTicketLen = 128

// ticketSecret signs the timestamp inside a minted ticket.
//
// PROCESS-LOCAL and random, deliberately. The signature exists so that "how
// long has this caller been trying" cannot be set by the caller — an unsigned
// or wrongly-signed ticket is still a perfectly good identity for grouping, it
// just gets no age credit. Losing the secret on restart therefore costs
// nothing but the aging of tickets minted before it, which is the honest
// outcome anyway: after a restart the queue those tickets were waiting in no
// longer exists.
var ticketSecret = func() []byte {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// A box that cannot produce randomness has worse problems, and a
		// constant here would only make signatures meaningless, not unsafe:
		// the worst case is that age credit becomes forgeable.
		return []byte("corrallm-ticket-fallback-secret")
	}
	return b
}()

// ticketFrom reads the caller's id, if it sent one.
func ticketFrom(r *http.Request) string {
	for _, h := range []string{HeaderRequestID, HeaderRequestIDAlt, HeaderTicket} {
		if v := strings.TrimSpace(r.Header.Get(h)); v != "" {
			if len(v) > maxTicketLen {
				v = v[:maxTicketLen]
			}
			return v
		}
	}
	return ""
}

// ensureTicket returns this request's ticket, minting one if it has none.
//
// The minted id is written back onto the request's own headers so that
// everything downstream of the rejection — the 429 body, the activity row —
// reports the SAME id without threading it through each call site. The request
// is per-call state that dies with the response; nothing else observes it.
func ensureTicket(r *http.Request) string {
	if t := ticketFrom(r); t != "" {
		return t
	}
	t := mintTicket(time.Now())
	r.Header.Set(HeaderRequestID, t)
	return t
}

// mintTicket builds a signed, self-describing id: tkt_<ms in base36>_<rand>_<sig>.
//
// The mint time is IN the id because that is what makes age free to compute.
// The alternative — remembering when each ticket was first seen — is a map that
// grows with traffic and has to be expired, consulted on the admission path,
// and kept correct across restarts, all to recover a number the caller is
// already carrying.
func mintTicket(now time.Time) string {
	ms := now.UnixMilli()
	nonce := make([]byte, 5)
	if _, err := rand.Read(nonce); err != nil {
		// Uniqueness degrades to the millisecond, which is enough to keep two
		// tickets apart in practice and is strictly better than refusing to
		// mint one.
		return ticketPrefix + strconv.FormatInt(ms, 36) + "_0_" + ticketSig(ms, "0")
	}
	n := hex.EncodeToString(nonce)
	return ticketPrefix + strconv.FormatInt(ms, 36) + "_" + n + "_" + ticketSig(ms, n)
}

func ticketSig(ms int64, nonce string) string {
	m := hmac.New(sha256.New, ticketSecret)
	m.Write([]byte(strconv.FormatInt(ms, 36) + ":" + nonce))
	return hex.EncodeToString(m.Sum(nil))[:12]
}

// ticketAge reports how long ago WE minted this ticket, and whether that can be
// trusted.
//
// ok is false for a caller-supplied id, a malformed one, or one whose signature
// does not verify — all of which are fine as identities and none of which may
// buy priority. It is also false for a ticket claiming to be from the future,
// which is either a clock problem or an attempt to look old.
func ticketAge(t string, now time.Time) (time.Duration, bool) {
	if !strings.HasPrefix(t, ticketPrefix) {
		return 0, false
	}
	parts := strings.Split(strings.TrimPrefix(t, ticketPrefix), "_")
	if len(parts) != 3 {
		return 0, false
	}
	ms, err := strconv.ParseInt(parts[0], 36, 64)
	if err != nil {
		return 0, false
	}
	if !hmac.Equal([]byte(ticketSig(ms, parts[1])), []byte(parts[2])) {
		return 0, false
	}
	age := now.Sub(time.UnixMilli(ms))
	if age < 0 {
		return 0, false
	}
	return age, true
}

// agedCtx tells the scheduler how long this caller has been trying, when the
// request carries a ticket we minted.
//
// Only OUR signed tickets buy age (see ticketAge): a caller-supplied id is a
// perfectly good identity for grouping the log, but letting it set the age
// would make queue priority a header anyone can write. A request with no
// ticket, or someone else's, gets the context unchanged and is scheduled
// exactly as it was before tickets existed.
func agedCtx(ctx context.Context, r *http.Request) context.Context {
	age, ok := ticketAge(ticketFrom(r), time.Now())
	if !ok || age <= 0 {
		return ctx
	}
	return sched.WithWaitingSince(ctx, time.Now().Add(-age))
}
