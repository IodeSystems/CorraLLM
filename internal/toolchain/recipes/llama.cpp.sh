#!/usr/bin/env bash
# llama.cpp recipe — probe | upstream | preflight | install-deps | build
#
# P25a implements everything except `build`, which is P25c and refuses loudly
# rather than half-building.

DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=./common.sh
source "$DIR/common.sh"

BIN_NAME=${TOOL_BIN:-llama-server}

# ---------------------------------------------------------------------------
# probe
#
# llama-server WRITES ITS VERSION TO STDERR and exits 0:
#
#   version: 10380 (0b1bad14f)
#   built with Clang 18.1.3 for Linux x86_64
#
# `v=$(llama-server --version)` therefore captures the empty string and reports
# "version unknown" on a perfectly good binary. Merging the streams is the whole
# trick, and it is why this is a recipe rather than a generic `--version` call.
# ---------------------------------------------------------------------------
probe() {
    local bindir; bindir=$(tool_bin_dir)
    local path="$bindir/$BIN_NAME"

    if [ ! -x "$path" ]; then
        printf '{"present":false,"path":%s,"version":"","commit":"","source":"","stamp":"","error":""}\n' \
            "$(jstr "$path")"
        return 0
    fi

    local banner version commit source stamp
    banner=$("$path" --version 2>&1 | head -2)
    # "version: 10380 (0b1bad14f)" → version "10380 (0b1bad14f)", commit "0b1bad14f"
    version=$(printf '%s' "$banner" | sed -n 's/^version: *\(.*\)$/\1/p' | head -1)
    commit=$(printf '%s' "$version" | sed -n 's/.*(\([0-9a-f]\{6,\}\)).*/\1/p' | head -1)
    stamp=$(stamp_read "$bindir")

    if [ -n "$version" ]; then
        source=binary
    elif [ -n "$stamp" ]; then
        version=$(stamp_field "$stamp" head)
        commit=$version
        source=stamp
    fi

    printf '{"present":true,"path":%s,"version":%s,"commit":%s,"source":%s,"stamp":%s,"error":""}\n' \
        "$(jstr "$path")" "$(jstr "$version")" "$(jstr "$commit")" \
        "$(jstr "$source")" "$(jstr "$stamp")"
}

# upstream compares the installed commit against the pin's remote head. Uses the
# binary's own commit when there is no stamp, so an ADOPTED ml-kit build still
# reports drift.
upstream() {
    local bindir; bindir=$(tool_bin_dir)
    local path="$bindir/$BIN_NAME" localrev=""
    if [ -x "$path" ]; then
        localrev=$("$path" --version 2>&1 | sed -n 's/.*(\([0-9a-f]\{6,\}\)).*/\1/p' | head -1)
    fi
    if [ -z "$localrev" ]; then
        localrev=$(stamp_field "$(stamp_read "$bindir")" head)
    fi
    emit_upstream "$localrev"
}

# ---------------------------------------------------------------------------
# preflight
#
# Seconds, never compiles. CUDA is deliberately NOT a hard requirement: llama.cpp
# builds CPU-only and that is a legitimate configuration (the Mac has no nvcc and
# is not broken). It is reported as a note so the absence is visible without
# being fatal.
# ---------------------------------------------------------------------------
preflight() {
    local missing_names=() missing_pkgs=() notes=() cmds=()

    have git   || { missing_names+=("git");   missing_pkgs+=("git"); }
    have cmake || { missing_names+=("cmake"); missing_pkgs+=("cmake"); }
    if ! have c++ && ! have g++ && ! have clang++; then
        missing_names+=("a C++ compiler")
        missing_pkgs+=("build-essential")
    fi

    if have nvidia-smi; then
        local archs
        archs=$(nvidia-smi --query-gpu=compute_cap --format=csv,noheader,nounits 2>/dev/null \
            | awk -F. 'NF==2 { printf "%d%d\n", $1, $2 }' | sort -un | paste -sd';')
        if [ -n "$archs" ]; then
            notes+=("CUDA build: targets arch(s) $archs — every card present contributes, so one binary serves a mixed box")
        fi
        if ! have nvcc && ! ls -d /usr/local/cuda-[0-9]*/bin/nvcc >/dev/null 2>&1; then
            notes+=("nvidia-smi present but no nvcc found — the build would fall back to CPU-only")
        fi
    else
        notes+=("no NVIDIA GPU detected — builds CPU-only, which is supported")
    fi

    local ok=1
    [ ${#missing_names[@]} -eq 0 ] || ok=0
    if [ ${#missing_pkgs[@]} -gt 0 ]; then
        local c; c=$(pkg_install_cmd "${missing_pkgs[@]}")
        [ -n "$c" ] && cmds+=("$c")
    fi

    printf '{"ok":%s,"runnable":true,"missing":%s,"packages":%s,"commands":%s,"notes":%s}\n' \
        "$(jbool "$ok")" \
        "$(jarr ${missing_names[@]+"${missing_names[@]}"})" \
        "$(jarr ${missing_pkgs[@]+"${missing_pkgs[@]}"})" \
        "$(jarr ${cmds[@]+"${cmds[@]}"})" \
        "$(jarr ${notes[@]+"${notes[@]}"})"
}

# install-deps runs what preflight said was missing. The agent refuses to call
# this at all unless started with --allow-install-deps; reaching here means the
# operator asked for it on this host, twice.
install_deps() {
    local pf; pf=$(preflight)
    local pkgs
    pkgs=$(printf '%s' "$pf" | sed -n 's/.*"packages":\[\([^]]*\)\].*/\1/p' | tr -d '"' | tr ',' ' ')
    if [ -z "${pkgs// /}" ]; then
        printf '{"ok":true,"ran":[],"error":""}\n'
        return 0
    fi
    # shellcheck disable=SC2086
    local cmd; cmd=$(pkg_install_cmd $pkgs)
    if [ -z "$cmd" ]; then
        printf '{"ok":false,"ran":[],"error":%s}\n' \
            "$(jstr "no supported package manager; install by hand: $pkgs")"
        return 0
    fi
    say "running: $cmd"
    if eval "$cmd" >&2; then
        printf '{"ok":true,"ran":%s,"error":""}\n' "$(jarr "$cmd")"
    else
        printf '{"ok":false,"ran":%s,"error":%s}\n' "$(jarr "$cmd")" "$(jstr "install failed; see log")"
    fi
}

case "${1:-}" in
    probe)        require_tool_root; probe ;;
    upstream)     require_tool_root; require_env TOOL_URL TOOL_REF; upstream ;;
    preflight)    preflight ;;
    install-deps) install_deps ;;
    build)        die "build is not implemented yet (P25c); this recipe answers probe, upstream, preflight and install-deps" ;;
    *)            die "unknown verb: ${1:-<none>}" ;;
esac
