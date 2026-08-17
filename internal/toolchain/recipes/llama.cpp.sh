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

# ---------------------------------------------------------------------------
# build
#
# Align → patch → configure → compile → install → stamp. Refuses on an adopted
# entry: TOOL_INSTALLED_AT means somebody else owns that tree, and the first
# thing a build does is `git clean -xdf`.
# ---------------------------------------------------------------------------
build() {
    local started=$SECONDS
    local src; src=$(tool_src_dir)
    local bindir; bindir=$(tool_bin_dir)

    if adopted; then
        die "refusing to build an adopted install ($TOOL_INSTALLED_AT) — corrallm does not write to a tree it does not own; drop installedAt to manage it here"
    fi

    # Patches are reverted BEFORE aligning, or align_tree sees them as the
    # human's uncommitted work and refuses to move the tree.
    unapply_patches "$src"
    align_tree "$src" "$TOOL_REF" "$TOOL_URL" || die "could not align $src to $TOOL_REF"
    apply_patches "$src" || die "patch set does not apply to $(current_head "$src")"

    local head archs stamp_now
    head=$(current_head "$src")
    archs=$(cuda_arch_spec)
    # The stamp carries HEAD *and* the patch hash *and* the arch list: `git
    # apply` does not move HEAD, and adding a GPU changes neither HEAD nor the
    # patches, so a HEAD-only stamp skips builds that are genuinely needed.
    stamp_now="head=$head patches=$(patch_set_hash) archs=$archs"

    if [ "${TOOL_FORCE:-0}" != "1" ] && [ -x "$bindir/$BIN_NAME" ] && [ "$(stamp_read "$bindir")" = "$stamp_now" ]; then
        say "up-to-date at $stamp_now; skipping build"
        printf '{"ok":true,"skipped":true,"head":%s,"stamp":%s,"seconds":%d,"error":""}\n' \
            "$(jstr "$head")" "$(jstr "$stamp_now")" "$((SECONDS - started))"
        return 0
    fi

    say "=== building $src (archs=$archs)"
    (
    cd "$src" || exit 1
    git clean -xdf >&2

    # Pin ONE toolchain for both languages.
    #
    # ggml's cmake/common.cmake computes its warning flags from a single
    # compiler id and applies the C set to C targets — so a box whose `cc` and
    # `c++` disagree gets one compiler's flags handed to the other. box1 is
    # exactly that box: `cc` is gcc 13.3 and `c++` is clang 18.1.3, which fails
    # the build at 2% with
    #   cc: error: unrecognized command-line option '-Wunreachable-code-break'
    # on ggml.c, sha256.c and friends. ml-kit's builder exports CC/CXX=clang and
    # never sees this; dropping that was what broke the first run here.
    #
    # An explicit CC/CXX from the caller always wins.
    if [ -z "${CC:-}" ] && [ -z "${CXX:-}" ] && have clang && have clang++; then
        CC=$(command -v clang); CXX=$(command -v clang++)
        export CC CXX
        say "  toolchain: $CC / $CXX (pinned; this host's cc and c++ disagree)"
    elif [ -n "${CC:-}" ] || [ -n "${CXX:-}" ]; then
        say "  toolchain: CC=${CC:-<default>} CXX=${CXX:-<default>} (from the environment)"
    fi

    local args=(-DCMAKE_BUILD_TYPE=Release)
    if have nvidia-smi && [ "$archs" != "cpu" ]; then
        nvidia-smi --query-gpu=name,compute_cap --format=csv,noheader 2>/dev/null | sed 's/^/  /' >&2
        args+=(
            -DGGML_CUDA=ON
            -DGGML_CUDA_F16=ON
            -DGGML_CUDA_FORCE_MMQ=ON
            -DCMAKE_CUDA_ARCHITECTURES="$archs"
            # The full matrix of KV-cache quantisation combinations for flash
            # attention. Not architecture-specific, but the configs here run
            # `-fa on` with quantised K/V, so a card built without it would be
            # missing exactly the kernels it needs.
            -DGGML_CUDA_FA_ALL_QUANTS=ON
        )
        local cuda_home; cuda_home=$(cuda_toolkit_home)
        if [ -n "$cuda_home" ]; then
            export CUDACXX="$cuda_home/bin/nvcc"
            export PATH="$cuda_home/bin:$PATH"
            export LD_LIBRARY_PATH="$cuda_home/lib64:${LD_LIBRARY_PATH:-}"
            say "  using $cuda_home/bin/nvcc ($("$cuda_home/bin/nvcc" --version | sed -n 's/.*release \([0-9.]*\).*/\1/p' | head -1))"
            args+=(
                -DCMAKE_CUDA_COMPILER="$cuda_home/bin/nvcc"
                -DCMAKE_CUDA_FLAGS="-I$cuda_home/include/"
                -DCMAKE_CXX_FLAGS="-I$cuda_home/include/"
            )
        fi
    else
        say "  no CUDA GPU detected; building CPU-only (supported)"
    fi

    cmake -B build "${args[@]}" >&2 || exit 1
    cmake --build build --config Release --parallel "$(nproc 2>/dev/null || echo 4)" >&2 || exit 1
    ) || die "build failed; see log"

    install_scope "$src/build/bin" "$bindir"
    stamp_write "$bindir" "$stamp_now"

    local version=""
    [ -x "$bindir/$BIN_NAME" ] && version=$("$bindir/$BIN_NAME" --version 2>&1 | sed -n 's/^version: *\(.*\)$/\1/p' | head -1)

    printf '{"ok":true,"skipped":false,"head":%s,"version":%s,"stamp":%s,"seconds":%d,"error":""}\n' \
        "$(jstr "$head")" "$(jstr "$version")" "$(jstr "$stamp_now")" "$((SECONDS - started))"
}

case "${1:-}" in
    probe)        require_tool_root; probe ;;
    upstream)     require_tool_root; require_env TOOL_URL TOOL_REF; upstream ;;
    build)        require_tool_root; require_env TOOL_URL TOOL_REF TOOL_PREFIX; build ;;
    preflight)    preflight ;;
    install-deps) install_deps ;;
    *)            die "unknown verb: ${1:-<none>}" ;;
esac
