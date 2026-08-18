#!/usr/bin/env bash
# ninfer recipe — probe | upstream | preflight | install-deps | build
#
# https://github.com/Neroued/ninfer — a from-scratch CUDA engine for a closed
# set of Qwen checkpoints, serving an OpenAI-compatible API. Two things make it
# unlike llama.cpp and both shape this recipe:
#
#  1. IT HAS NO --version. Not a flag, not a string anywhere in apps/ or src/.
#     Its version can come only from the stamp corrallm writes at build time, so
#     a ninfer built by hand elsewhere is genuinely unidentifiable and is
#     reported as present with an unknown version rather than a guessed one.
#  2. IT BUILDS FOR sm_120a ONLY. CMakeLists hard-pins CMAKE_CUDA_ARCHITECTURES
#     to 120a and FATAL_ERRORs on anything else. So "can this host build it" is a
#     real question with a real no — and buildable is not the same as runnable:
#     a box with a 5090 and a 3080 can build it and can only run it on the 5090.

DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=./common.sh
source "$DIR/common.sh"

BIN_NAME=${TOOL_BIN:-ninfer-serve}

# NInfer's floors, from its CMakeLists. Named here so a failure says "cmake is
# too old" instead of surfacing as an inscrutable configure error.
MIN_CMAKE=3.28
MIN_CUDA=13.1
REQUIRED_ARCH=120a

version_ge() { [ "$(printf '%s\n%s\n' "$2" "$1" | sort -V | head -1)" = "$2" ]; }

cmake_version() { have cmake && cmake --version 2>/dev/null | head -1 | awk '{print $3}'; }

# nvcc_bin resolves the toolkit to MEASURE, and deliberately does not trust PATH
# first.
#
# box1 has two: /usr/bin/nvcc is the distro's CUDA 12.0 from 2023 and shadows
# /usr/local/cuda-13.3/bin/nvcc in PATH. Trusting `command -v nvcc` there
# reports 12.0 and fails ninfer's >= 13.1 floor on a machine that has 13.3
# installed and working — a wrong "you cannot build this" is worse than no
# answer, because it sends you installing a toolkit you already have.
#
# Same resolution order as ml-kit's cuda_toolkit_home, which learned this first:
# explicit CUDA_HOME, then CUDA_VERSION, then the newest /usr/local toolkit, and
# only then whatever PATH happens to offer.
nvcc_bin() {
    if [ -n "${CUDA_HOME:-}" ] && [ -x "$CUDA_HOME/bin/nvcc" ]; then
        printf '%s' "$CUDA_HOME/bin/nvcc"; return
    fi
    if [ -n "${CUDA_VERSION:-}" ] && [ -x "/usr/local/cuda-$CUDA_VERSION/bin/nvcc" ]; then
        printf '%s' "/usr/local/cuda-$CUDA_VERSION/bin/nvcc"; return
    fi
    local d
    for d in $(printf '%s\n' /usr/local/cuda-[0-9]* 2>/dev/null | sort -Vr); do
        [ -x "$d/bin/nvcc" ] && { printf '%s' "$d/bin/nvcc"; return; }
    done
    have nvcc && command -v nvcc
}

nvcc_version() {
    local n; n=$(nvcc_bin)
    [ -n "$n" ] || return 0
    "$n" --version 2>/dev/null | sed -n 's/.*release \([0-9.]*\).*/\1/p' | head -1
}

# has_sm120 reports whether a card this engine can RUN on is present. Distinct
# from buildability: nvcc cross-compiles for an absent architecture happily.
has_sm120() {
    have nvidia-smi || return 1
    nvidia-smi --query-gpu=compute_cap --format=csv,noheader,nounits 2>/dev/null \
        | grep -q '^12\.0'
}

ffmpeg_packages() {
    case "$(pkg_manager)" in
        apt)    printf 'libavformat-dev libavcodec-dev libavutil-dev libswscale-dev' ;;
        dnf)    printf 'ffmpeg-devel' ;;
        pacman) printf 'ffmpeg' ;;
        brew)   printf 'ffmpeg' ;;
        *)      printf '' ;;
    esac
}

# probe: the stamp is the only version source. No banner to parse.
probe() {
    local bindir; bindir=$(tool_bin_dir)
    local path="$bindir/$BIN_NAME"

    if [ ! -x "$path" ]; then
        printf '{"present":false,"path":%s,"version":"","commit":"","source":"","stamp":"","error":""}\n' \
            "$(jstr "$path")"
        return 0
    fi

    local stamp head source="" version=""
    stamp=$(stamp_read "$bindir")
    head=$(stamp_field "$stamp" head)
    if [ -n "$head" ]; then
        version=$head
        source=stamp
    fi
    # No else. A ninfer we did not build cannot be identified, and inventing a
    # version for it would make the registry lie about the one tool whose
    # version is hardest to establish.
    printf '{"present":true,"path":%s,"version":%s,"commit":%s,"source":%s,"stamp":%s,"error":""}\n' \
        "$(jstr "$path")" "$(jstr "$version")" "$(jstr "$head")" "$(jstr "$source")" "$(jstr "$stamp")"
}

upstream() {
    local bindir; bindir=$(tool_bin_dir)
    emit_upstream "$(stamp_field "$(stamp_read "$bindir")" head)"
}

preflight() {
    local missing_names=() missing_pkgs=() notes=() cmds=()

    have git || { missing_names+=("git"); missing_pkgs+=("git"); }

    local cv; cv=$(cmake_version)
    if [ -z "$cv" ]; then
        missing_names+=("cmake >= $MIN_CMAKE")
        missing_pkgs+=("cmake")
    elif ! version_ge "$cv" "$MIN_CMAKE"; then
        missing_names+=("cmake >= $MIN_CMAKE (have $cv)")
    else
        # Worth saying out loud when it is exactly at the floor: a routine
        # distro change breaks the build with a message that will not obviously
        # mean "cmake".
        [ "$cv" = "$MIN_CMAKE" ] || version_ge "$cv" "3.29" || \
            notes+=("cmake $cv is at ninfer's minimum ($MIN_CMAKE) — a downgrade breaks the build")
    fi

    local nv nb; nv=$(nvcc_version); nb=$(nvcc_bin)
    if [ -z "$nv" ]; then
        missing_names+=("CUDA toolkit >= $MIN_CUDA (no nvcc found)")
    elif ! version_ge "$nv" "$MIN_CUDA"; then
        missing_names+=("CUDA >= $MIN_CUDA (have $nv at $nb)")
    fi
    # A shadowed toolkit is worth naming even when the answer is fine: the build
    # and a hand-run cmake would disagree about which nvcc they mean.
    if [ -n "$nb" ] && have nvcc; then
        local pathnvcc; pathnvcc=$(command -v nvcc)
        if [ "$pathnvcc" != "$nb" ]; then
            notes+=("using $nb ($nv); PATH's nvcc is $pathnvcc ($("$pathnvcc" --version 2>/dev/null | sed -n 's/.*release \([0-9.]*\).*/\1/p' | head -1)) and is NOT what this build would use")
        fi
    fi

    have pkg-config || { missing_names+=("pkg-config"); missing_pkgs+=("pkg-config"); }

    if have pkg-config && ! pkg-config --exists libavformat libavcodec libavutil libswscale 2>/dev/null; then
        missing_names+=("ffmpeg development libraries (libavformat/libavcodec/libavutil/libswscale)")
        local fp; fp=$(ffmpeg_packages)
        # shellcheck disable=SC2206
        [ -n "$fp" ] && missing_pkgs+=($fp)
    fi

    local runnable=0
    if has_sm120; then
        runnable=1
    else
        notes+=("no sm_120 device present — ninfer builds for $REQUIRED_ARCH only and will not run on this host's GPUs")
    fi

    local ok=1
    [ ${#missing_names[@]} -eq 0 ] || ok=0
    if [ ${#missing_pkgs[@]} -gt 0 ]; then
        local c; c=$(pkg_install_cmd "${missing_pkgs[@]}")
        [ -n "$c" ] && cmds+=("$c")
    fi

    printf '{"ok":%s,"runnable":%s,"missing":%s,"packages":%s,"commands":%s,"notes":%s}\n' \
        "$(jbool "$ok")" "$(jbool "$runnable")" \
        "$(jarr ${missing_names[@]+"${missing_names[@]}"})" \
        "$(jarr ${missing_pkgs[@]+"${missing_pkgs[@]}"})" \
        "$(jarr ${cmds[@]+"${cmds[@]}"})" \
        "$(jarr ${notes[@]+"${notes[@]}"})"
}

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
# Unlike llama.cpp this has ONE valid architecture. CMakeLists hard-pins
# CMAKE_CUDA_ARCHITECTURES to 120a and FATAL_ERRORs on anything else, so there
# is no arch detection to do — and no point building for "every card present",
# because only one of box1's two cards can run the result.
#
# The stamp still records the arch for the same reason llama.cpp's does: it is
# part of what the binary IS, and a stamp that omits it would let a changed
# target skip a rebuild.
# ---------------------------------------------------------------------------
build() {
    local started=$SECONDS
    local src; src=$(tool_src_dir)
    local bindir; bindir=$(tool_bin_dir)

    if adopted; then
        die "refusing to build an adopted install ($TOOL_INSTALLED_AT) — corrallm does not write to a tree it does not own; drop installedAt to manage it here"
    fi

    unapply_patches "$src"
    align_tree "$src" "$TOOL_REF" "$TOOL_URL" || die "could not align $src to $TOOL_REF"
    apply_patches "$src" || die "patch set does not apply to $(current_head "$src")"

    local head stamp_now
    head=$(current_head "$src")
    stamp_now="head=$head patches=$(patch_set_hash) archs=$REQUIRED_ARCH"

    if [ "${TOOL_FORCE:-0}" != "1" ] && [ -x "$bindir/$BIN_NAME" ] && [ "$(stamp_read "$bindir")" = "$stamp_now" ]; then
        say "up-to-date at $stamp_now; skipping build"
        printf '{"ok":true,"skipped":true,"head":%s,"stamp":%s,"seconds":%d,"error":""}\n' \
            "$(jstr "$head")" "$(jstr "$stamp_now")" "$((SECONDS - started))"
        return 0
    fi

    local nvcc; nvcc=$(nvcc_bin)
    [ -n "$nvcc" ] || die "no nvcc found; ninfer needs CUDA >= $MIN_CUDA"

    say "=== building $src (arch=$REQUIRED_ARCH, $nvcc)"
    (
    cd "$src" || exit 1
    git clean -xdf >&2

    export CUDACXX="$nvcc"
    export PATH="$(dirname "$nvcc"):$PATH"

    cmake -B build \
        -DCMAKE_BUILD_TYPE=Release \
        -DCMAKE_CUDA_ARCHITECTURES="$REQUIRED_ARCH" \
        -DCMAKE_CUDA_COMPILER="$nvcc" \
        -DNINFER_BUILD_APPS=ON >&2 || exit 1
    cmake --build build --config Release --parallel "$(nproc 2>/dev/null || echo 4)" >&2 || exit 1
    ) || die "build failed; see log"

    # ninfer has no install target and does not collect its binaries, so they
    # are gathered by name rather than by copying a bin/ that does not exist.
    mkdir -p "$bindir"
    local found=0 f
    for f in ninfer ninfer-serve; do
        local built
        built=$(find "$src/build" -type f -name "$f" -perm -u+x 2>/dev/null | head -1)
        if [ -n "$built" ]; then
            cp -f "$built" "$bindir/$f"
            found=$((found + 1))
            say "  installed $f"
        fi
    done
    [ "$found" -gt 0 ] || die "build produced no ninfer binaries under $src/build"

    stamp_write "$bindir" "$stamp_now"

    # No --version to ask, so the stamp's head IS the version. See the note at
    # the top of this recipe.
    printf '{"ok":true,"skipped":false,"head":%s,"version":%s,"stamp":%s,"seconds":%d,"error":""}\n' \
        "$(jstr "$head")" "$(jstr "$head")" "$(jstr "$stamp_now")" "$((SECONDS - started))"
}

case "${1:-}" in
    probe)        require_tool_root; probe ;;
    upstream)     require_tool_root; require_env TOOL_URL TOOL_REF; upstream ;;
    preflight)    preflight ;;
    install-deps) install_deps ;;
    build)        require_tool_root; require_env TOOL_NAME TOOL_URL TOOL_REF; build ;;
    *)            die "unknown verb: ${1:-<none>}" ;;
esac
