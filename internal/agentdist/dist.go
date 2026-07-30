// Package agentdist serves the agent binary and its installer from the primary.
//
// The point is iteration speed. Getting a change onto an attached machine
// otherwise means walking to it, and a laptop that is asleep or elsewhere makes
// that a real obstacle to trying anything. Here the daemon is the distribution
// point: build once on the box that already has the toolchain, and every agent
// picks it up on its own.
//
// These routes are PUBLIC, deliberately. They sit outside /api/ so an unenrolled
// machine can fetch the installer and the binary — the same posture /v1 and
// /health already have. Nothing here is a credential: the binary is the same one
// you can build from the repo, and it does nothing until it is given a token.
// The gate is the enrollment token the installer is handed, not the download.
package agentdist

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/iodesystems/corrallm/internal/agent"
)

// Handler serves the installer and the per-platform agent binaries out of dir
// (conventionally <repo>/bin/agents, populated by `make agents`).
type Handler struct {
	Dir string
	// Version is the PRIMARY's own version, used only as a fallback.
	Version string
}

// servedVersion is the version stamped into the binaries in Dir, written by
// `make agents` alongside them.
//
// It must be the BINARIES' version, not the primary's. The two are built by
// separate targets and routinely disagree — a primary built with a plain `go
// build` reports "dev" while the served binaries carry a commit — and an agent
// comparing itself against the primary's number would re-exec into a build it
// is already running, find the versions still differ, and update forever.
// ServedVersion is servedVersion, exported for the heartbeat.
func (h *Handler) ServedVersion() string { return h.servedVersion() }

func (h *Handler) servedVersion() string {
	b, err := os.ReadFile(filepath.Join(h.Dir, "VERSION"))
	if err != nil {
		return h.Version
	}
	if v := strings.TrimSpace(string(b)); v != "" {
		return v
	}
	return h.Version
}

// InstallScript renders the bootstrap a machine pipes into bash.
//
// It detects the platform itself rather than trusting the caller, refuses
// anything it has no binary for, and writes a config file rather than baking
// the token into a shell history — the token is the membership credential and
// deserves a file with sane permissions.
func (h *Handler) InstallScript(base string) string {
	return `#!/bin/sh
# corrallm agent installer.
#
#   curl -fsSL ` + base + `/install.sh | sh -s -- --token <token>
#
# Installs into ./corrallm-agent in the current directory, the way GitHub's
# runner does: everything the agent needs in one folder you can see, move or
# delete, rather than scattered through your home directory.
#
# POSIX sh, and what it writes is POSIX sh too — no bashisms, nothing that
# assumes your login shell. The agent reads its own YAML config rather than
# needing an environment arranged by the caller, because "set -a; . file; set +a"
# is bash syntax and does not work in fish.
#
# The agent runs shell commands sent by the primary. Install it only on a
# machine you intend to hand over for that purpose.
set -eu

PRIMARY="` + base + `"
SERVER=""
TOKEN=""
DIR="./corrallm-agent"
ADDR=":6503"

while [ $# -gt 0 ]; do
  case "$1" in
    --server) SERVER="$2"; shift 2 ;;
    --token)  TOKEN="$2";  shift 2 ;;
    --addr)   ADDR="$2";   shift 2 ;;
    --dir)    DIR="$2";    shift 2 ;;
    -h|--help)
      echo "usage: install.sh --token <token> [--server <name>] [--addr :6503] [--dir ./corrallm-agent]"
      exit 0 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done

[ -n "$TOKEN" ] || { echo "--token is required (mint one in the dashboard)" >&2; exit 2; }

OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)
case "$ARCH" in
  x86_64|amd64) ARCH=amd64 ;;
  arm64|aarch64) ARCH=arm64 ;;
  *) echo "unsupported architecture: $ARCH" >&2; exit 1 ;;
esac
case "$OS" in
  darwin|linux) ;;
  *) echo "unsupported OS: $OS (windows is not supported)" >&2; exit 1 ;;
esac

mkdir -p "$DIR"
echo "==> downloading corrallm-$OS-$ARCH"
curl -fsSL "$PRIMARY/install/corrallm-$OS-$ARCH" -o "$DIR/corrallm.new"
chmod +x "$DIR/corrallm.new"
mv "$DIR/corrallm.new" "$DIR/corrallm"

# An enrollment token (enr_...) is exchanged on first start for a long-lived
# credential, which the agent writes back into this same file. Anything else is
# treated as an already-issued agent token.
umask 077
case "$TOKEN" in
  enr_*) TOKEN_KEY="enrollToken" ;;
  *)     TOKEN_KEY="token" ;;
esac
cat > "$DIR/agent.yml" <<YML
# corrallm agent. Rewritten by the agent after enrollment.
primary: $PRIMARY
server: $SERVER
$TOKEN_KEY: $TOKEN
addr: "$ADDR"
selfUpdate: true
YML

# Quoted heredoc: the launcher is written verbatim, no expansion by this script.
# The text comes from the agent package so the installer and the agent's own
# self-update cannot write different launchers.
cat > "$DIR/` + agent.LauncherName + `" <<'SH'
` + agent.LauncherScript + `SH
chmod +x "$DIR/` + agent.LauncherName + `"

echo "==> installed $DIR/"
echo "      corrallm    the agent binary"
echo "      agent.yml   its configuration (contains a credential; mode 0600)"
echo "      start.sh    run this — it supervises the agent and restarts it"
echo
echo "Start it:"
echo "  $DIR/start.sh"
echo
echo "On first start it enrols with $PRIMARY, which sizes this machine from its own"
echo "memory probe and writes the long-lived credential back into agent.yml. After"
echo "that it heartbeats and self-updates while idle."
`
}

// Mount registers the public install routes on mux.
func (h *Handler) Mount(get func(pattern string, fn http.HandlerFunc), base func(*http.Request) string) {
	get("/install.sh", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(h.InstallScript(base(r))))
	})
	get("/install/{name}", h.serveBinary)
}

// serveBinary hands over one platform's agent binary.
func (h *Handler) serveBinary(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	// The name indexes a fixed set of built artifacts; reject anything that
	// could escape the directory rather than sanitising it.
	if name == "" || strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") ||
		!strings.HasPrefix(name, "corrallm-") {
		http.Error(w, "unknown artifact", http.StatusNotFound)
		return
	}
	p := filepath.Join(h.Dir, name)
	f, err := os.Open(p)
	if err != nil {
		http.Error(w, fmt.Sprintf("no built agent for %q — run `make agents` on the primary", name), http.StatusNotFound)
		return
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		http.Error(w, "unreadable artifact", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	// The version of THIS artifact, so an agent can tell whether installing it
	// would change anything before it restarts into it.
	w.Header().Set("X-Corrallm-Version", h.servedVersion())
	http.ServeContent(w, r, name, st.ModTime(), f)
}
