package proxy

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"syscall"

	"github.com/iodesystems/corrallm/internal/proc"
)

// typeBackendRestarting is the error type of a 503 for a local backend that is
// stopping, starting or not yet listening. Documented in errorContract.
const typeBackendRestarting = "backend_restarting"

// backendRestarting reports whether a transport error from a LOCAL backend
// means "it is going down or not up yet" rather than "it answered wrongly".
//
// It is called only before any byte of the reply reached the caller
// (ReverseProxy's ErrorHandler runs before the response is written; a stream
// that breaks later is cut, not re-answered). For a loopback backend the
// cases are:
//
//   - corrallm ordered the stop (drain, restart, eviction): any error.
//   - connection refused: nothing listens on the port — a backend that was
//     killed from outside, or one still loading. Nothing reached the model.
//   - EOF / reset before the reply: the process closed the connection, which
//     a llama-server does on SIGTERM; a crash looks the same.
//
// A malformed reply ("malformed HTTP response", a bad chunk) stays a 502: the
// backend is up and broken, and a retry would not help.
func backendRestarting(err error, pr *proc.Process) bool {
	if pr.Stopping() {
		return true
	}
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// writeBackendRestarting answers 503 + Retry-After for a backend that
// backendRestarting says is down for now.
func writeBackendRestarting(w http.ResponseWriter, model string, cause error) {
	secs := retryAfterSecs(proc.BackendRestartRetryAfter)
	h := w.Header()
	h.Set("Retry-After", strconv.Itoa(secs))
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Del("Content-Length")
	w.WriteHeader(http.StatusServiceUnavailable)
	msg := "backend " + model + " is restarting or not yet listening (" + cause.Error() + "); no reply was sent; retry after " + strconv.Itoa(secs) + "s"
	_, _ = io.WriteString(w, `{"error":{"message":`+jsonString(msg)+`,"type":"`+typeBackendRestarting+`","retry_after":`+strconv.Itoa(secs)+`}}`)
}
