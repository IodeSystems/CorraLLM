package agent

import (
	"log/slog"
	"os"
	"path/filepath"
	"slices"
)

// LauncherName is the launcher's filename, written next to the agent binary.
const LauncherName = "start.sh"

// LauncherScript is how THIS build expects to be started.
//
// It lives in the binary rather than only in the installer so that a self-update
// carries its own launcher: the agent replaces ./corrallm and re-execs, and the
// new image then writes the start.sh that matches it. Serving start.sh from the
// primary instead would let the two drift — a launcher fetched separately from
// the binary it launches is a version skew waiting to happen.
//
// POSIX sh, no bashisms: this has to run identically whether the operator's
// login shell is bash, zsh or fish.
//
// The `cd` is load-bearing. Self-update re-execs with the same argv and inherits
// cwd, so `./corrallm` and `--config ./agent.yml` only keep resolving because
// the process is already sitting in its install directory.
const LauncherScript = `#!/bin/sh
# Supervise the corrallm agent. POSIX sh — runs the same from bash, zsh or fish.
#
# A loop rather than a bare exec, because self-update re-execs in place and keeps
# no old process in reserve: the new image releases the listen port and re-binds
# it from scratch, and if it loses that race the agent is simply gone until
# somebody returns to the machine. Surviving that is the whole reason this is a
# supervisor.
#
# Self-update stays invisible here — it replaces the image behind an unchanged
# PID, so the wait below never notices.
#
# Deliberately no "set -e": a supervisor that exits on the first command
# returning nonzero is not a supervisor.
cd "$(dirname "$0")" || exit 1

delay=1
max=60
child=

# Forward a stop to the agent and wait for it out. A launcher that died without
# doing this would orphan a live agent, and that orphan still holds the port the
# next start needs — turning a stop into a machine that cannot come back.
stop() {
  if [ -n "$child" ]; then kill -TERM "$child" 2>/dev/null; fi
  wait
  exit 0
}
trap stop INT TERM

while :; do
  started=$(date +%s)
  ./corrallm agent --config ./agent.yml "$@" &
  child=$!
  wait "$child"
  code=$?
  child=

  # Exit 0 is the agent shutting down on purpose. Follow it out rather than
  # restarting something that was asked to stop.
  if [ "$code" -eq 0 ]; then exit 0; fi

  # A run that lasted a while was healthy; do not make it inherit the backoff
  # earned by an earlier crash loop, or one flaky week leaves every later
  # restart pinned at the cap.
  if [ $(($(date +%s) - started)) -ge 60 ]; then delay=1; fi

  echo "corrallm agent exited $code; restarting in ${delay}s" >&2
  sleep "$delay"
  delay=$((delay * 2))
  if [ "$delay" -gt "$max" ]; then delay=$max; fi
done
`

// pastLaunchers are the launcher texts that EARLIER builds wrote.
//
// This is what makes the launcher updatable without clobbering an operator who
// edited theirs. A start.sh matching any text corrallm has ever generated is
// ours to replace; anything else is assumed to be a local change and is left
// alone. Without the list the only options are "never update" or "silently
// overwrite whatever is there", and the second one eats someone's edit.
//
// When LauncherScript changes, append the OLD text here — never edit or remove
// an entry, or installs still carrying it become unupgradable. Byte-exact,
// trailing newline included.
var pastLaunchers = []string{
	// The original: a bare exec, with nothing left to restart the agent if a
	// self-update came up and could not re-bind its port.
	`#!/bin/sh
# Start the corrallm agent. POSIX sh — runs the same from bash, zsh or fish.
cd "$(dirname "$0")" || exit 1
exec ./corrallm agent --config ./agent.yml "$@"
`,
}

// ReconcileLauncher brings dir/start.sh in line with LauncherScript.
//
// Called on every start, not only after an update: an agent that re-execs into a
// new build is the same process with a new image, so "on start" is exactly when
// the new launcher text becomes available. Idempotent — it writes only when the
// content actually differs.
//
// A missing start.sh is not an error and is NOT created. Its absence means this
// is not an installer-made directory (a dev running ./bin/corrallm from the
// repo, say), and dropping a start.sh into someone's build output would be
// litter, not a feature.
func ReconcileLauncher(dir string) error {
	path := filepath.Join(dir, LauncherName)
	have, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if string(have) == LauncherScript {
		return nil
	}
	if !isKnownLauncher(string(have)) {
		slog.Info("agent: start.sh does not match any build's launcher; assuming local edits and leaving it alone",
			"path", path)
		return nil
	}

	// Temp-and-rename, and here it is not merely about torn writes. start.sh is
	// a SUPERVISOR now, so the shell running it is alive for the agent's whole
	// lifetime — and shells read their script lazily, by offset. Writing this
	// file in place would have that shell resume at a byte offset into different
	// content and execute garbage. Rename swaps the directory entry while the
	// running shell keeps its open fd on the old inode, exactly as the binary
	// swap does, so the supervisor finishes the script it started.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(LauncherScript), 0o755); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	slog.Info("agent: start.sh updated to this build's launcher", "path", path)
	return nil
}

func isKnownLauncher(s string) bool {
	return slices.Contains(pastLaunchers, s)
}
