#!/usr/bin/env bash
# Build corrallm from nothing, in a container that has nothing.
#
# A shell script rather than Make recipe lines because this needs real control
# flow. Each line of a Make recipe is its OWN shell, so an `exit 0` meant as
# "skip the rest" only ends that one line and the next runs anyway — which is
# exactly how the first version of this ran `git archive` inside a container
# with no .git.
set -uo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
cd "$root"

REF="${VERIFY_REF:-HEAD}"
GO_VERSION=$(awk '/^go /{print $2; exit}' go.mod)
TAG="corrallm-verify:${GO_VERSION}"

say() { printf '%s\n' "$*"; }

# Both of these are "cannot run this check", not "this check failed". Saying so
# beats a red build on a laptop without docker, and beats pretending the check
# passed when it never ran.
if ! command -v docker >/dev/null 2>&1; then
    say "==> docker check SKIPPED — no docker here."
    say "    This machine cannot prove the isolated build. Install docker, or run"
    say "    'make verify-deps VERIFY_DOCKER=0' to accept local-only checks."
    exit 0
fi
if ! git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    say "==> docker check SKIPPED — not a git work tree."
    say "    The context is 'git archive', so there is nothing to archive. This is"
    say "    the normal case INSIDE the verification container itself."
    exit 0
fi

say "==> building from a bare ubuntu:24.04 (go ${GO_VERSION})"
say "    context: git archive ${REF} — what is COMMITTED, not the working tree."
say "    Uncommitted work is not tested here; that is the point. To check a change"
say "    before committing it:  git add -A && make verify-docker VERIFY_REF=\$(git write-tree)"

if git archive "$REF" | docker build \
        --build-arg "GO_VERSION=${GO_VERSION}" \
        -f scripts/verify/Dockerfile \
        -t "$TAG" - ; then
    say ""
    say "==> isolated build OK — a fresh clone on a bare Ubuntu can build this."
    exit 0
fi

# Guidance, not just a red X. Each cause maps to one fix, in the order they
# actually occur.
cat >&2 <<'GUIDE'

  THE ISOLATED BUILD FAILED. Read the last failing step above, then:

    "command not found" / "no such file or directory" running a tool
        A system dependency this project needs and this machine happened to
        have. Add the package to scripts/verify/Dockerfile — WITH a comment
        saying why — so the next person inherits the answer instead of the
        search.

    a Go package cannot be resolved, or a version is missing an API
        Something builds here only because go.work or a `replace` points at a
        sibling checkout. Release that dependency and pin the released version;
        `make verify-deps` checks the pin, this proves it.

    a file is missing that plainly exists locally
        It is untracked. The context is `git archive`, so anything not committed
        is invisible here — which is the same thing a fresh clone sees.

    the tests fail here but pass locally
        Something in them assumes this machine: a path, a GPU, a running
        service, a tool on PATH. That is a real finding about the tests.

GUIDE
exit 1
