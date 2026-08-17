#!/usr/bin/env bash
# Shared helpers for tool recipes. Sourced, never run directly.
#
# THE OUTPUT CONTRACT. A recipe prints human progress on stderr and exactly one
# JSON object on stdout, as its last act. The agent parses stdout and streams
# stderr, so anything chatty written to stdout corrupts the result — every
# informational echo in a recipe must be `>&2`.

set -uo pipefail

# json_escape renders one string as a JSON string body (no surrounding quotes).
# Handles the cases that actually occur here: Windows-style CR in a version
# banner, tabs and newlines in multi-line --version output, and backslashes in
# paths.
json_escape() {
    local s=${1-}
    s=${s//\\/\\\\}
    s=${s//\"/\\\"}
    s=${s//$'\r'/}
    s=${s//$'\t'/\\t}
    s=${s//$'\n'/\\n}
    printf '%s' "$s"
}

# jstr emits a quoted JSON string.
jstr() { printf '"%s"' "$(json_escape "${1-}")"; }

# jarr emits its arguments as a JSON array of strings.
jarr() {
    local out="[" first=1 a
    for a in "$@"; do
        [ $first -eq 1 ] || out+=","
        out+=$(jstr "$a")
        first=0
    done
    printf '%s]' "$out"
}

# jbool emits true/false from a shell-ish truth value.
jbool() {
    case "${1-}" in
        1|true|yes|on) printf 'true' ;;
        *) printf 'false' ;;
    esac
}

# have reports whether a command exists.
have() { command -v "$1" >/dev/null 2>&1; }

# say prints progress. ALWAYS stderr — see the output contract above.
say() { printf '%s\n' "$*" >&2; }

# die emits a JSON object carrying an error and exits non-zero. Recipes use it
# for "this verb cannot answer", which is different from "the answer is no" —
# the latter is a normal result with ok:false.
die() {
    printf '{"error":%s}\n' "$(jstr "$*")"
    exit 1
}

# ---------------------------------------------------------------------------
# Package manager
#
# Detected, never assumed. An unknown manager reports the packages it would have
# installed and stops; guessing at a command that mangles a system is strictly
# worse than saying "I do not know how to install these here".
# ---------------------------------------------------------------------------

pkg_manager() {
    if have apt-get; then printf 'apt'
    elif have dnf; then printf 'dnf'
    elif have pacman; then printf 'pacman'
    elif have brew; then printf 'brew'
    else printf ''
    fi
}

# pkg_install_cmd prints the command that would install the given packages.
# Empty output means no supported manager.
#
# sudo is prepended only when we are not already root AND sudo exists. A recipe
# that hardcodes sudo fails inside a container that has neither.
pkg_install_cmd() {
    local mgr; mgr=$(pkg_manager)
    [ -n "$mgr" ] || return 0
    local pre=""
    if [ "$(id -u)" != "0" ] && [ "$mgr" != "brew" ] && have sudo; then
        pre="sudo "
    fi
    case "$mgr" in
        apt)    printf '%sapt-get install -y %s' "$pre" "$*" ;;
        dnf)    printf '%sdnf install -y %s' "$pre" "$*" ;;
        pacman) printf '%spacman -S --needed --noconfirm %s' "$pre" "$*" ;;
        brew)   printf 'brew install %s' "$*" ;;
    esac
}

# ---------------------------------------------------------------------------
# Stamp
#
# What corrallm remembers about a build it performed. Carried verbatim from
# ml-kit's `.built-from`, including WHY each field is in it:
#
#   head    — the commit built.
#   patches — hash of the applied patch set. `git apply` does not move HEAD, so
#             a head-only stamp skips the build after a patch edit and you test
#             the old binary believing it is the new one.
#   archs   — the CUDA arch list. Adding a GPU changes neither head nor patches,
#             so without this the build is skipped and the binary cannot see the
#             new card.
#
# It is also the ONLY version source for a tool that cannot report its own
# version (ninfer has no --version at all), which is why it is written even when
# nothing would skip a build.
# ---------------------------------------------------------------------------

STAMP_NAME=".corrallm-stamp"

stamp_path() { printf '%s/%s' "${1}" "$STAMP_NAME"; }

stamp_read() {
    local p; p=$(stamp_path "$1")
    [ -f "$p" ] || return 0
    tr -d '\n' < "$p"
}

stamp_write() {
    local dir=$1; shift
    mkdir -p "$dir"
    printf '%s' "$*" > "$(stamp_path "$dir")"
}

# stamp_field pulls one `k=v` field out of a stamp string.
stamp_field() {
    local stamp=$1 key=$2 tok
    for tok in $stamp; do
        case "$tok" in
            "$key"=*) printf '%s' "${tok#*=}"; return ;;
        esac
    done
}

# require_env fails CLEANLY when the caller left something out.
#
# Without it `set -u` aborts a command substitution mid-printf and the recipe
# emits `{"ref":,...}` — malformed JSON, which the agent reports as a parse
# failure and which says nothing about the actual mistake. A recipe's error path
# has to be valid JSON too; that is the whole reason the contract is a JSON
# object rather than an exit code.
require_env() {
    local v missing=()
    for v in "$@"; do
        [ -n "${!v:-}" ] || missing+=("$v")
    done
    [ ${#missing[@]} -eq 0 ] || die "missing required environment: ${missing[*]}"
}

# ---------------------------------------------------------------------------
# Paths
#
# TOOL_PREFIX is this tool's root under corrallm's home: src/ is the managed
# checkout, bin/ is what gets installed and what a ${tool:} reference resolves
# to. TOOL_INSTALLED_AT, when set, ADOPTS an existing install elsewhere: probe
# reads it, and nothing ever writes there.
# ---------------------------------------------------------------------------

# require_tool_root insists on somewhere to look. A managed tool has
# TOOL_PREFIX; an adopted one has TOOL_INSTALLED_AT. Neither means the caller
# has not said which install it is asking about, and probing "/ninfer-serve"
# would answer "not present" about a path that was never the question.
require_tool_root() {
    [ -n "${TOOL_PREFIX:-}" ] || [ -n "${TOOL_INSTALLED_AT:-}" ] || \
        die "missing required environment: TOOL_PREFIX or TOOL_INSTALLED_AT"
}

tool_bin_dir() {
    if [ -n "${TOOL_INSTALLED_AT:-}" ]; then
        printf '%s' "$TOOL_INSTALLED_AT"
    else
        printf '%s/bin' "${TOOL_PREFIX:?TOOL_PREFIX not set}"
    fi
}

tool_src_dir() { printf '%s/src' "${TOOL_PREFIX:?TOOL_PREFIX not set}"; }

# adopted reports whether this host's entry points at someone else's install.
adopted() { [ -n "${TOOL_INSTALLED_AT:-}" ]; }

# ---------------------------------------------------------------------------
# Upstream
#
# One `git ls-remote` round trip. No clone, no fetch — the scheduled drift check
# runs this on every tool on every host and must stay cheap.
# ---------------------------------------------------------------------------

remote_head() {
    local url=$1 ref=$2 out
    out=$(git ls-remote "$url" "$ref" 2>/dev/null | head -1 | cut -f1)
    [ -n "$out" ] || out=$(git ls-remote "$url" "refs/heads/$ref" 2>/dev/null | head -1 | cut -f1)
    printf '%s' "$out"
}

# emit_upstream compares what we have against what upstream has.
#
# `local` is the commit we can prove is installed, from the stamp when corrallm
# built it and from the binary's own banner when it did not. That second path is
# what makes drift visible on an ADOPTED install on day one — llama-server
# prints its short commit, so a build corrallm never performed can still be told
# it is behind.
emit_upstream() {
    local localrev=$1
    local rhead; rhead=$(remote_head "${TOOL_URL:?}" "${TOOL_REF:?}")
    local behind=0
    if [ -n "$rhead" ] && [ -n "$localrev" ]; then
        # Compare on the shorter of the two: a banner carries an abbreviated
        # hash and ls-remote a full one, and they are equal when one prefixes
        # the other.
        local n=${#localrev}
        [ "$n" -gt 0 ] && [ "${rhead:0:$n}" != "$localrev" ] && behind=1
    fi
    printf '{"ref":%s,"remoteHead":%s,"local":%s,"behind":%s,"error":%s}\n' \
        "$(jstr "$TOOL_REF")" "$(jstr "$rhead")" "$(jstr "$localrev")" \
        "$(jbool "$behind")" \
        "$(jstr "$([ -z "$rhead" ] && printf 'could not reach %s' "$TOOL_URL")")"
}
