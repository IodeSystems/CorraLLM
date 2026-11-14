#!/bin/sh
# Install corrallm (the CLIENT-SIDE installer, served from the website):
#   curl -fsSL https://corrallm.iodesystems.com/install.sh | sh
#
# NOT the daemon's /install.sh (internal/agentdist), which a running corrallm
# serves to its own agents. This script is site/install.sh in the repository.
#
# Downloads the latest release for this OS and CPU from GitHub, verifies the
# release's SHA256SUMS file with minisign, checks the archive's sha256 against
# it, and puts corrallm, llm-bench and llm-bench-mcp in ~/.local/bin
# (CORRALLM_INSTALL_DIR overrides). Runs nothing else: no service, no build.
#
# Environment:
#   CORRALLM_INSTALL_DIR   target directory (default $HOME/.local/bin)
#   CORRALLM_UNVERIFIED=1  skip the signature check (sha256 only, against an unverified file)
#   CORRALLM_ALLOW_ROOT=1  allow running as root
#   CORRALLM_BASE_URL      release download URL prefix (testing; default: the latest GitHub release)
set -eu

dir=${CORRALLM_INSTALL_DIR:-$HOME/.local/bin}
base=${CORRALLM_BASE_URL:-https://github.com/IodeSystems/CorraLLM/releases/latest/download}
src=https://github.com/IodeSystems/CorraLLM

fail() { echo "install.sh: $*" >&2; exit 1; }

if [ "$(id -u)" = 0 ] && [ "${CORRALLM_ALLOW_ROOT:-}" != 1 ]; then
  fail "refusing to run as root; run as your own user, or set CORRALLM_ALLOW_ROOT=1"
fi

case $(uname -s) in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) fail "no release for $(uname -s): releases are built for Linux and macOS only; build from source: $src" ;;
esac
case $(uname -m) in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) fail "no release for $(uname -m); build from source: $src" ;;
esac
if [ "$os" = darwin ] && [ "$arch" = amd64 ]; then
  fail "no release for macOS on Intel (darwin/amd64 is not built); build from source: $src"
fi
plat=${os}_${arch}
asset=corrallm_$plat.tar.gz

if command -v curl >/dev/null 2>&1; then
  get() { curl -fsSL -o "$2" "$1"; }
elif command -v wget >/dev/null 2>&1; then
  get() { wget -qO "$2" "$1"; }
else
  fail "needs curl or wget"
fi
if command -v sha256sum >/dev/null 2>&1; then
  sum() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
  sum() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
  fail "needs sha256sum or shasum"
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

get "$base/SHA256SUMS" "$tmp/SHA256SUMS" || fail "could not download $base/SHA256SUMS (a release without the stable-name assets, or no network)"

# Release key, shared with dun and para (key ID 2CC27A171DE03D42). Same as minisign.pub on this site.
cat > "$tmp/minisign.pub" <<'KEY'
untrusted comment: minisign public key 2CC27A171DE03D42
RWRCPeAdF3rCLFMIlOWIL4mfNDKFhvF8YL3PVH3iOO35Uh6pYdqoCvDR
KEY
verified=0
if [ "${CORRALLM_UNVERIFIED:-}" = 1 ]; then
  echo "warning: CORRALLM_UNVERIFIED=1: the signature is NOT checked; the sha256 only matches the archive to an unverified SHA256SUMS" >&2
elif command -v minisign >/dev/null 2>&1; then
  get "$base/SHA256SUMS.minisig" "$tmp/SHA256SUMS.minisig" || fail "could not download the SHA256SUMS signature"
  minisign -V -q -p "$tmp/minisign.pub" -m "$tmp/SHA256SUMS" -x "$tmp/SHA256SUMS.minisig" \
    || fail "the SHA256SUMS signature does not verify; nothing was installed"
  echo "SHA256SUMS signature verified (key 2CC27A171DE03D42)"
  verified=1
else
  fail "minisign is not installed, so the release signature cannot be checked. Install minisign (apt install minisign, brew install minisign) and run this again, or set CORRALLM_UNVERIFIED=1 to install without the check"
fi

# The signature's trusted comment binds the product and version (the key is shared with dun and para).
version=
if [ "$verified" = 1 ]; then
  tc=$(sed -n 's/^trusted comment: //p' "$tmp/SHA256SUMS.minisig" | head -1)
  case $tc in
    "corrallm v"*" SHA256SUMS") version=${tc#corrallm }; version=${version% SHA256SUMS} ;;
    *) fail "the signature is for '$tc', not a corrallm release" ;;
  esac
fi

want=$(awk -v f="$asset" '$2 == f || $2 == "*" f { print $1; exit }' "$tmp/SHA256SUMS")
[ -n "$want" ] || fail "SHA256SUMS lists no $asset (this release has no $plat build)"

echo "corrallm ${version:+$version }for $plat"
get "$base/$asset" "$tmp/$asset" || fail "could not download $asset"
got=$(sum "$tmp/$asset")
[ "$got" = "$want" ] || fail "sha256 mismatch for $asset: got $got, SHA256SUMS says $want"

mkdir "$tmp/x"
tar -xzf "$tmp/$asset" -C "$tmp/x" corrallm llm-bench llm-bench-mcp
mkdir -p "$dir"
# Temp file in the target directory, then rename: a running binary is replaced
# whole, never half-written (a plain copy over a running executable fails with ETXTBSY).
for b in corrallm llm-bench llm-bench-mcp; do
  [ -f "$tmp/x/$b" ] || fail "the archive has no $b"
  cp "$tmp/x/$b" "$dir/.$b.new.$$"
  chmod 755 "$dir/.$b.new.$$"
  mv -f "$dir/.$b.new.$$" "$dir/$b"
  echo "installed $dir/$b"
done

case ":$PATH:" in
  *":$dir:"*) ;;
  *) echo "note: $dir is not on PATH; add it, e.g. export PATH=\"$dir:\$PATH\"" ;;
esac
cat <<'NEXT'

next (nothing has been started):
  1. Write a config: corrallm.yaml with your servers, models and keys.
     Example and field reference: https://github.com/IodeSystems/CorraLLM#configure
  2. Install the service (systemd user unit on Linux, launchd LaunchAgent on macOS):
       corrallm service install -- --config /path/to/corrallm.yaml
     Everything after -- is passed to `corrallm serve`.
  3. Linux: keep the user service running after logout:
       loginctl enable-linger "$USER"

To run local models, corrallm starts llama.cpp's llama-server, which you build or install yourself:
  - a C/C++ compiler, cmake and git
  - NVIDIA GPU: the CUDA toolkit and the driver
  - macOS: Metal needs nothing extra (Xcode command line tools provide the compiler)
Paid upstreams (OpenAI-compatible endpoints) need none of this.
NEXT
