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

# cpu_count is how many jobs a compile may run.
#
# `nproc` is GNU coreutils and does not exist on macOS, where the old fallback
# quietly built with -j4 on a machine with twelve cores — a build that looked
# merely slow rather than misconfigured.
cpu_count() {
    if have nproc; then nproc
    elif have sysctl; then sysctl -n hw.ncpu 2>/dev/null || printf 4
    else printf 4
    fi
}

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

# require_tool_root insists on somewhere to look: an adopted install to read, or
# a tool name from which the managed default can be derived.
require_tool_root() {
    [ -n "${TOOL_NAME:-}" ] || [ -n "${TOOL_INSTALLED_AT:-}" ] || \
        die "missing required environment: TOOL_NAME or TOOL_INSTALLED_AT"
}

# tool_prefix is where a MANAGED install lives, decided HERE rather than by the
# primary.
#
# The primary used to compute this from its own home directory and send it to
# every host, which is right for itself and wrong for anybody else: a Mac agent
# would have been told to install under /home/nthalk when its home is
# /Users/nthalk. Only the machine doing the installing knows where its home is,
# so an empty TOOL_PREFIX means "your default" and an explicit one (config's
# per-host `prefix:`) still wins.
tool_prefix() {
    if [ -n "${TOOL_PREFIX:-}" ]; then printf '%s' "$TOOL_PREFIX"; return; fi
    printf '%s/tools/%s' "${CORRALLM_HOME:-$HOME/.corrallm}" "${TOOL_NAME:?}"
}

tool_bin_dir() {
    if [ -n "${TOOL_INSTALLED_AT:-}" ]; then
        printf '%s' "$TOOL_INSTALLED_AT"
    else
        printf '%s/bin' "$(tool_prefix)"
    fi
}

tool_src_dir() { printf '%s/src' "$(tool_prefix)"; }

# adopted reports whether this host's entry points at someone else's install.
adopted() { [ -n "${TOOL_INSTALLED_AT:-}" ]; }

# ---------------------------------------------------------------------------
# Build support — aligning a tree to its pin, carrying local patches, and
# deciding whether a build is needed at all.
#
# Ported from ml-kit's bin/llama-rebuild. Each rule below exists because its
# absence produced a wrong result, not because it seemed tidy.
# ---------------------------------------------------------------------------

current_head() { (cd "$1" && git rev-parse HEAD 2>/dev/null); }

patch_dir() { printf '%s/patches' "$(tool_prefix)"; }

# patch_files emits this tool's patches in apply order.
patch_files() {
    local pd; pd=$(patch_dir)
    [ -d "$pd" ] || return 0
    find "$pd" -maxdepth 1 -name '*.patch' -type f 2>/dev/null | LC_ALL=C sort
}

# patch_set_hash identifies the patch CONTENT, so editing a patch invalidates
# the stamp. "none" when there are no patches.
patch_set_hash() {
    local files; files=$(patch_files)
    [ -z "$files" ] && { printf 'none'; return; }
    # shellcheck disable=SC2086
    cat $files | sha256sum | cut -c1-12
}

# unapply_patches reverts our patches so align_tree sees a pristine tree.
#
# Load-bearing: `git reset --hard` would destroy them anyway, and align_tree
# REFUSES to move a tree with uncommitted tracked changes — which applied
# patches are. Left alone, the tree freezes at the patched commit and silently
# stops taking upstream. Best-effort by design: a patch that will not reverse
# cleanly is left, and align_tree's own guard then warns instead of clobbering.
unapply_patches() {
    local dir=$1 f
    [ -d "$dir" ] || return 0
    while IFS= read -r f; do
        [ -n "$f" ] || continue
        if (cd "$dir" && git apply -R --check "$f" >/dev/null 2>&1); then
            (cd "$dir" && git apply -R "$f")
        fi
    done < <(patch_files | tac)
}

# apply_patches applies the set idempotently. An already-applied patch is not an
# error (the reverse-check proves it); one that neither applies nor reverses is
# fatal, because building an unpatched tree you believe is patched is worse than
# not building at all.
apply_patches() {
    local dir=$1 f n=0
    local files; files=$(patch_files)
    [ -z "$files" ] && return 0
    while IFS= read -r f; do
        [ -n "$f" ] || continue
        if (cd "$dir" && git apply --check "$f" >/dev/null 2>&1); then
            (cd "$dir" && git apply "$f"); say "    applied  $(basename "$f")"
        elif (cd "$dir" && git apply -R --check "$f" >/dev/null 2>&1); then
            say "    already  $(basename "$f")"
        else
            say "    FAILED   $(basename "$f")"
            say "  Upstream moved under this patch. Refresh or drop it; refusing to"
            say "  build a tree that is not in the state the patch set describes."
            return 1
        fi
        n=$((n + 1))
    done <<< "$files"
    say "  $n patch(es) in effect"
    return 0
}

# align_tree clones if needed and resets to the pinned ref.
#
# It will NOT clobber a tree with uncommitted tracked changes. Untracked files
# (build output, .idea) are left alone by reset --hard anyway, so the guard is
# on tracked changes only.
align_tree() {
    local dir=$1 ref=$2 url=$3
    if [ ! -d "$dir/.git" ]; then
        say "=== cloning $url -> $dir"
        mkdir -p "$(dirname "$dir")"
        git clone "$url" "$dir" >&2 || return 1
    fi
    (
    cd "$dir" || exit 1
    # `git remote get-url` APPLIES url.<base>.insteadOf rewrites, so on a box
    # that rewrites https://github.com/ to git@github.com: it returns the ssh
    # form while the stored value is the https one we set. Comparing against it
    # makes this branch fire on every single run and "update" the origin to the
    # value it already has. Read the raw config instead.
    local cur; cur=$(git config --get remote.origin.url 2>/dev/null)
    if [ -n "$cur" ] && [ "$cur" != "$url" ]; then
        say "  updating origin: $cur -> $url"
        git remote set-url origin "$url"
    fi
    if ! git diff --quiet || ! git diff --cached --quiet; then
        say "  WARNING: $dir has uncommitted tracked changes; leaving as-is (not aligning to $ref)"
        say "  at $(git rev-parse --short HEAD) $(git log -1 --format='%s')"
        exit 0
    fi
    say "  fetching $ref"
    git fetch --tags origin "$ref" >&2 || exit 1
    git reset --hard FETCH_HEAD >&2 || exit 1
    say "  at $(git rev-parse --short HEAD) $(git log -1 --format='%s')"
    )
}

# cuda_toolkit_home resolves the toolkit to BUILD with.
#
# Explicit CUDA_HOME wins, then CUDA_VERSION, then the newest /usr/local
# toolkit that has an nvcc. PATH is deliberately last: box1 carries a distro
# /usr/bin/nvcc at CUDA 12.0 that shadows 13.3, and building against it would
# silently produce a binary from the wrong toolkit. Prints nothing when there is
# no toolkit, and cmake then finds its own.
cuda_toolkit_home() {
    if [ -n "${CUDA_HOME:-}" ]; then
        [ -x "$CUDA_HOME/bin/nvcc" ] || { say "CUDA_HOME=$CUDA_HOME has no bin/nvcc"; return 1; }
        printf '%s' "$CUDA_HOME"; return
    fi
    if [ -n "${CUDA_VERSION:-}" ]; then
        local want="/usr/local/cuda-$CUDA_VERSION"
        [ -x "$want/bin/nvcc" ] || { say "CUDA_VERSION=$CUDA_VERSION: no nvcc at $want/bin/nvcc"; return 1; }
        printf '%s' "$want"; return
    fi
    local d
    for d in $(printf '%s\n' /usr/local/cuda-[0-9]* 2>/dev/null | sort -Vr); do
        [ -x "$d/bin/nvcc" ] && { printf '%s' "$d"; return; }
    done
}

# cuda_arch_spec resolves the architectures to target.
#
# EVERY card present contributes, so a mixed box (a 5090 beside a 3080) produces
# ONE binary that runs on both. An earlier `head -n 1` saw only the first card
# and silently built for one architecture, leaving the other unable to load the
# backend at all. CUDA_ARCHS overrides, and is the only way to target a card that
# is not installed yet — detection can only describe the hardware in the box now.
cuda_arch_spec() {
    if [ -n "${CUDA_ARCHS:-}" ]; then printf '%s' "$CUDA_ARCHS"; return; fi
    have nvidia-smi || { printf 'cpu'; return; }
    local a
    a=$(nvidia-smi --query-gpu=compute_cap --format=csv,noheader,nounits 2>/dev/null \
        | awk -F. 'NF==2 { printf "%d%d\n", $1, $2 }' | sort -un | paste -sd';')
    printf '%s' "${a:-native}"
}

# ---------------------------------------------------------------------------
# Versioned installs
#
# A build no longer overwrites the one that is serving. Each build lands in its
# own directory and `bin` is a SYMLINK naming the active one:
#
#   <prefix>/builds/20260823-224500-c060ca97/llama-server
#   <prefix>/bin -> builds/20260823-224500-c060ca97
#
# Three properties fall out of that, and each is one this used to lack:
#
#   1. The running build survives the next one. `rm -rf $dst` used to delete it
#      before the copy, so a build that died mid-install left no working binary
#      at all — on the very path every model spawns against.
#   2. A bad build is reversible. Upstream ships several llama.cpp builds a day
#      and any of them can regress a model; `activate` puts the previous one
#      back in the time it takes to rename a symlink, with no compile.
#   3. The swap is atomic. `bin` moves from one complete tree to another by
#      rename(2), so a spawn racing an install sees the old build or the new
#      one, never a half-copied directory.
#
# The symlink target is RELATIVE (`builds/<id>`), so the whole prefix can be
# moved or copied to another machine without every link dangling.
#
# `bin` is still the only path anything outside here knows: probe reports
# through it and ${tool:x} resolves to it, so a rollback needs no config edit
# and no restart — the next spawn follows the link to wherever it now points.
# ---------------------------------------------------------------------------

# BUILD_KEEP is how many builds survive a prune. Five is enough to walk back
# through a bad week of upstream and small enough that a 400 MB llama.cpp tree
# does not quietly eat a laptop's disk.
build_keep() { printf '%s' "${TOOL_KEEP_BUILDS:-5}"; }

builds_dir() { printf '%s/builds' "$(tool_prefix)"; }

# build_id names a build: sortable, and carrying the commit so `builds` is
# readable without opening a stamp.
build_id() {
    local head=${1-}
    printf '%s' "$(date -u +%Y%m%d-%H%M%S)${head:+-${head:0:8}}"
}

# mtime prints a file's modification time as a unix timestamp. GNU and BSD stat
# share no flags at all, so both are tried — this runs on box1 and on a Mac.
mtime() { stat -c %Y "$1" 2>/dev/null || stat -f %m "$1" 2>/dev/null; }

# active_build names the build `bin` points at, empty when the layout is not
# versioned (an adopted install, or one from before this existed).
active_build() {
    local link; link="$(tool_prefix)/bin"
    [ -L "$link" ] || return 0
    basename "$(readlink "$link")"
}

# versioned reports whether this prefix uses the symlink layout.
versioned() { [ -L "$(tool_prefix)/bin" ]; }

# migrate_legacy_bin moves a pre-versioning install into builds/ and links to
# it, so the FIRST versioned build still has a predecessor to roll back to.
#
# Runs at build time rather than on a read: a listing that mutates the tree it
# is describing is a surprise, and a build is already the moment this prefix
# changes shape. Moving the directory does not disturb a running process — it
# holds the inode, not the path.
migrate_legacy_bin() {
    local prefix; prefix=$(tool_prefix)
    local bin="$prefix/bin"
    [ -d "$bin" ] && [ ! -L "$bin" ] || return 0
    local head id
    head=$(stamp_field "$(stamp_read "$bin")" head)
    id="legacy-$(build_id "$head")"
    say "=== adopting the existing install as builds/$id"
    mkdir -p "$prefix/builds"
    mv "$bin" "$prefix/builds/$id" || return 1
    (cd "$prefix" && ln -s "builds/$id" bin)
}

# swap_link renames one symlink onto another WITHOUT following it.
#
# `mv tmp bin` is wrong here and quietly so: when bin is a symlink to a
# directory, both GNU and BSD mv follow it and move tmp INSIDE that directory —
# so the link never moves, the old build stays active, and a stray `.bin.new.N`
# link appears inside it. The tests caught exactly that.
#
# The flag that says "rename the link itself" is spelled differently on each:
# -T on GNU, -h on BSD. Try both, and fall back to unlink-then-rename on
# anything that has neither, which is not atomic but is still correct.
swap_link() {
    local tmp=$1 dst=$2
    mv -Tf "$tmp" "$dst" 2>/dev/null && return 0
    mv -hf "$tmp" "$dst" 2>/dev/null && return 0
    if [ -d "$dst" ] && [ ! -L "$dst" ]; then
        say "  refusing to replace the directory $dst with a link"
        rm -f "$tmp"
        return 1
    fi
    rm -f "$dst" && mv -f "$tmp" "$dst"
}

# activate_build points `bin` at one build, atomically.
activate_build() {
    local id=$1 prefix; prefix=$(tool_prefix)
    [ -d "$prefix/builds/$id" ] || { say "no such build: $id"; return 1; }
    migrate_legacy_bin || return 1
    (
    cd "$prefix" || exit 1
    # Symlink then rename, rather than `ln -sfn`: -f unlinks before it links,
    # which leaves a window with no bin/ at all, and its behaviour on a link to
    # a directory differs between GNU and BSD. rename(2) has neither problem.
    ln -s "builds/$id" ".bin.new.$$" || exit 1
    swap_link ".bin.new.$$" bin || { rm -f ".bin.new.$$"; exit 1; }
    ) || return 1
}

# install_build copies a finished build into its own directory and activates it.
install_build() {
    local src=$1 id=$2 prefix; prefix=$(tool_prefix)
    local dst="$prefix/builds/$id"
    migrate_legacy_bin || die "could not migrate the existing install at $prefix/bin"
    say "=== installing -> $dst"
    rm -rf "$dst"
    mkdir -p "$dst"
    cp -a "$src"/. "$dst"/ || die "could not install the build output into $dst"
    activate_build "$id" || die "built and installed $id, but could not point $prefix/bin at it"
}

# prune_builds keeps the newest N, and NEVER the active one even if it has aged
# out of the window: retention is about disk, and deleting what is currently
# serving to satisfy a count would be a self-inflicted outage.
prune_builds() {
    local dir; dir=$(builds_dir)
    [ -d "$dir" ] || return 0
    local keep active i=0 b
    keep=$(build_keep)
    active=$(active_build)
    for b in $(ls -1t "$dir" 2>/dev/null); do
        i=$((i + 1))
        [ "$i" -le "$keep" ] && continue
        if [ "$b" = "$active" ]; then
            say "  keeping $b past the retention window: it is the active build"
            continue
        fi
        say "  pruning $b"
        rm -rf "${dir:?}/$b"
    done
}

# list_builds reports what can be rolled back to, newest first.
list_builds() {
    local dir; dir=$(builds_dir)
    local active; active=$(active_build)
    local out="[" first=1 b stamp head at
    if [ -d "$dir" ]; then
        for b in $(ls -1t "$dir" 2>/dev/null); do
            [ -d "$dir/$b" ] || continue
            stamp=$(stamp_read "$dir/$b")
            head=$(stamp_field "$stamp" head)
            at=$(mtime "$dir/$b")
            [ $first -eq 1 ] || out+=","
            out+=$(printf '{"id":%s,"stamp":%s,"head":%s,"at":%s,"active":%s}' \
                "$(jstr "$b")" "$(jstr "$stamp")" "$(jstr "$head")" "${at:-0}" \
                "$(jbool "$([ "$b" = "$active" ] && printf 1)")")
            first=0
        done
    fi
    out+="]"
    printf '{"builds":%s,"active":%s,"versioned":%s,"keep":%s,"error":""}\n' \
        "$out" "$(jstr "$active")" "$(jbool "$(versioned && printf 1)")" "$(build_keep)"
}

# builds_verb and activate_verb are the same on every tool — where a build
# landed is a property of the layout, not of what was compiled — so they live
# here and each recipe just dispatches to them.
builds_verb() {
    adopted && die "$TOOL_INSTALLED_AT is adopted: corrallm did not build it and keeps no versions of it"
    list_builds
}

activate_verb() {
    adopted && die "$TOOL_INSTALLED_AT is adopted: corrallm does not repoint an install it does not own"
    local id=${TOOL_BUILD_ID:-}
    [ -n "$id" ] || die "no build id given"
    local prev; prev=$(active_build)
    if [ "$id" = "$prev" ]; then
        printf '{"ok":true,"active":%s,"previous":%s,"error":""}\n' "$(jstr "$id")" "$(jstr "$prev")"
        return 0
    fi
    activate_build "$id" || die "could not activate $id"
    printf '{"ok":true,"active":%s,"previous":%s,"error":""}\n' "$(jstr "$id")" "$(jstr "$prev")"
}

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
