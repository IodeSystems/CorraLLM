package agent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// selfUpdate replaces this agent's binary with the primary's build and restarts
// into it — but ONLY while nothing is running here.
//
// The idle condition is the whole design. An agent supervises process groups
// holding tens of GB; restarting while one is live would either strand it (the
// new process has no handles for it) or kill someone's warm model to ship a
// code change. Neither is an acceptable price for a deploy, so an agent that is
// busy simply stays on the old build and updates at the next quiet moment.
//
// Triggered by the heartbeat rather than a timer: the heartbeat is already the
// channel that knows the primary's version, and an agent that cannot reach its
// primary should not be replacing its own binary on the strength of stale
// information.
func (b *Beacon) maybeSelfUpdate(ctx context.Context, ack HeartbeatAck) {
	if !b.SelfUpdate || ack.Version == "" || ack.UpdateURL == "" {
		return
	}
	if b.Srv == nil || b.Srv.version == ack.Version {
		return
	}
	// "dev" on both sides is the normal state of an untagged build and would
	// otherwise never match; the primary changing while both are "dev" is not
	// something we can detect from a version string alone.
	if ack.Version == "dev" && b.Srv.version == "dev" {
		return
	}
	if busy := b.Srv.busy(); busy > 0 {
		slog.Info("agent: update available but backends are running; deferring",
			"have", b.Srv.version, "want", ack.Version, "backends", busy)
		return
	}

	exe, err := os.Executable()
	if err != nil {
		slog.Warn("agent: cannot locate own binary; skipping self-update", "err", err)
		return
	}
	slog.Info("agent: idle and out of date — updating",
		"have", b.Srv.version, "want", ack.Version, "path", exe)

	served, err := b.downloadInto(ctx, ack.UpdateURL, exe)
	if err != nil {
		slog.Error("agent: self-update failed; staying on the current build", "err", err)
		return
	}
	// The primary reports ITS version, but serves binaries built separately
	// (`make agents`), and the two can disagree — a primary built without the
	// version ldflag says "dev" while the served binary is stamped with a
	// commit. Believing the primary alone would re-exec into the same build,
	// find the versions still differ on the next beat, and update forever.
	//
	// The served binary's own version is the authority. If it matches what is
	// already running, there is nothing to restart into.
	if served != "" && served == b.Srv.version {
		slog.Info("agent: primary reports a different version but serves the build we already run; not restarting",
			"running", b.Srv.version, "primaryReports", ack.Version)
		return
	}

	// Re-exec rather than exit: the supervisor that started us may not restart
	// us, and an agent that vanishes to apply an update has made things worse.
	// Nothing is running (checked above), so there is nothing to hand over.
	slog.Info("agent: restarting into the new build", "version", ack.Version)
	if err := syscall.Exec(exe, os.Args, os.Environ()); err != nil {
		slog.Error("agent: exec into the new build FAILED — still running the old one", "err", err)
	}
}

// updateTempPrefix names the in-flight download. Distinctive and dot-prefixed:
// it shares a directory with the operator's own files, so it must be obviously
// ours before anything here deletes by pattern.
const updateTempPrefix = ".corrallm-update-"

// sweepStaleDownloads removes leftovers from updates that never finished.
//
// downloadInto cleans up after itself, but only if it returns — a process killed
// mid-download (which is precisely what happens on the flaky links this feature
// exists for) leaves its temp file behind. Each is a partial copy of a ~37 MB
// binary, so a machine that has been failing to update for months quietly turns
// gigabytes of its install directory into garbage nothing else will ever remove.
//
// Anything matching the prefix is safe to delete: updates are driven serially
// from the heartbeat and never overlap, and the current attempt's file does not
// exist yet. Failures are logged, not returned — being unable to tidy up is no
// reason to refuse an update.
func sweepStaleDownloads(dir string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if e.IsDir() || !strings.HasPrefix(e.Name(), updateTempPrefix) {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if err := os.Remove(p); err != nil {
			slog.Warn("agent: could not remove a stale update download", "path", p, "err", err)
			continue
		}
		slog.Info("agent: removed a stale update download", "path", p)
	}
}

// downloadInto fetches url into the file at dst, atomically.
//
// Written to a sibling temp file and renamed, so an interrupted download can
// never leave a truncated binary where the agent expects an executable — the
// failure mode there is a machine that cannot start at all, needing hands on
// it, which is exactly what this feature exists to avoid.
// Returns the served binary's own version stamp (from X-Corrallm-Version), so
// the caller can tell whether restarting would actually change anything.
func (b *Beacon) downloadInto(ctx context.Context, url, dst string) (string, error) {
	full := strings.TrimRight(b.Primary, "/") + url
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return "", err
	}
	cli := &http.Client{Timeout: 3 * time.Minute} // a ~37 MB binary over a slow link
	resp, err := cli.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
		return "", fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	served := resp.Header.Get("X-Corrallm-Version")

	sweepStaleDownloads(filepath.Dir(dst))

	tmp, err := os.CreateTemp(filepath.Dir(dst), updateTempPrefix+"*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once renamed

	n, err := io.Copy(tmp, resp.Body)
	if err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	// A binary is megabytes; anything tiny is an error page that arrived with a
	// 200, and installing it would brick the agent.
	if n < 1<<20 {
		return "", fmt.Errorf("downloaded %d bytes — too small to be an agent binary", n)
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return "", err
	}
	return served, os.Rename(tmpName, dst)
}

// busy reports how many backends this agent is currently supervising that have
// not exited.
func (s *Server) busy() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, b := range s.backends {
		select {
		case <-b.handle.Done():
		default:
			n++
		}
	}
	return n
}
