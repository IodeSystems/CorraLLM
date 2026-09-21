#!/usr/bin/env bash
# stable-diffusion.cpp recipe — probe | upstream | preflight | install-deps | build
#
# https://github.com/leejet/stable-diffusion.cpp — the image half of the same
# ggml/GGUF family llama.cpp belongs to, and the reason corrallm can serve
# Qwen-Image-2.1 without adapting anything: its `sd-server` already speaks
# OpenAI's /v1/images/generations, so corrallm proxies it rather than translates.
#
# THREE THINGS MAKE IT UNLIKE llama.cpp, and each one is a build that fails (or
# a version that reads "unknown") if the recipe pretends otherwise:
#
#  1. IT HAS SUBMODULES, AND ggml IS ONE. A plain clone leaves ggml/ an EMPTY
#     DIRECTORY and the configure dies in cmake/ggml.cmake. common.sh's
#     align_tree only clones and resets, so this recipe updates them itself —
#     see sync_submodules, which also explains why leaving it to the next build
#     would freeze the tree permanently.
#  2. NO BACKEND IS ON BY DEFAULT. llama.cpp's cmake turns Metal on for Apple by
#     itself; sd.cpp declares `option(SD_METAL ... OFF)` and every other backend
#     the same way. A Mac build with no flags is a CPU build that looks like a
#     GPU one, so the flag is passed explicitly and named in the stamp.
#  3. sd-server WRITES ITS VERSION TO STDOUT, which is the OPPOSITE of
#     llama-server. See probe.

DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=./common.sh
source "$DIR/common.sh"

BIN_NAME=${TOOL_BIN:-sd-server}

# ggml/CMakeLists.txt opens `cmake_minimum_required(VERSION 3.14...3.28)`. The
# root project asks for only 3.12, so the real floor is in the submodule and a
# host that meets the visible one still fails to configure. Named here so the
# answer is "cmake is too old" rather than an error from a directory the
# operator has not noticed exists yet.
MIN_CMAKE=3.14

version_ge() { [ "$(printf '%s\n%s\n' "$2" "$1" | sort -V | head -1)" = "$2" ]; }

cmake_version() { have cmake && cmake --version 2>/dev/null | head -1 | awk '{print $3}'; }

# backend_spec names what this host BUILDS FOR, and goes in the stamp.
#
# llama.cpp's recipe records the CUDA arch list there because adding a GPU
# changes neither HEAD nor the patch set and must still force a rebuild. The
# same argument applies to the backend itself here, and more strongly: nothing
# about sd.cpp's default configuration is GPU, so "cpu" and "metal" are two
# different binaries from one commit with one patch set. A stamp that called
# both of them `archs=cpu` — which is what cuda_arch_spec answers on a Mac —
# would skip the build that turns Metal on.
backend_spec() {
    if have nvidia-smi && [ "$(cuda_arch_spec)" != "cpu" ]; then
        printf 'cuda:%s' "$(cuda_arch_spec)"
    elif [ "$(uname -s)" = "Darwin" ]; then
        printf 'metal'
    else
        printf 'cpu'
    fi
}

# ---------------------------------------------------------------------------
# probe
#
# sd-server prints to STDOUT and exits 0:
#
#   stable-diffusion.cpp version master-889-c678dfe, commit c678dfe
#
# That is the opposite of llama-server, which prints the same kind of banner to
# STDERR — so the fix that recipe needs (merging the streams) is not the fix
# this one needs, and copying either blindly gets one of them wrong. Both
# streams are read anyway and the line is matched by its PREFIX: merging costs
# nothing while the anchored match keeps a stray log line from being mistaken
# for a version, so the answer survives sd.cpp moving the banner either way.
#
# `--version` is only honoured as argv[1] (examples/server/main.cpp returns
# before it parses anything else), which is why it is passed alone.
#
# The version half is `git describe --tags --abbrev=7 --dirty=+` baked in at
# configure time, so a tree carrying corrallm's patches reports a trailing `+`
# and says so on its own. The commit half is `git rev-parse --short HEAD`.
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
    banner=$("$path" --version 2>&1 | sed -n 's/^stable-diffusion\.cpp version //p' | head -1)
    # "master-889-c678dfe, commit c678dfe" → version "master-889-c678dfe", commit "c678dfe"
    version=$(printf '%s' "$banner" | sed -n 's/^\([^,]*\).*/\1/p')
    commit=$(printf '%s' "$banner" | sed -n 's/.*commit \([0-9a-f]\{6,\}\).*/\1/p')
    stamp=$(stamp_read "$bindir")

    # `git describe` is ERROR_QUIET in sd.cpp's cmake and the version macro
    # falls back to the literal "unknown" — a tagless or shallow tree gets a
    # real commit and a useless version. Prefer the commit over repeating a
    # word that says nothing.
    [ "$version" = "unknown" ] && version=$commit

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

# upstream compares the installed commit against the pin's remote head, taking
# the binary's own commit when there is no stamp so an install corrallm did not
# perform still reports drift.
upstream() {
    local bindir; bindir=$(tool_bin_dir)
    local path="$bindir/$BIN_NAME" localrev=""
    if [ -x "$path" ]; then
        localrev=$("$path" --version 2>&1 \
            | sed -n 's/^stable-diffusion\.cpp version .*commit \([0-9a-f]\{6,\}\).*/\1/p' | head -1)
    fi
    if [ -z "$localrev" ]; then
        localrev=$(stamp_field "$(stamp_read "$bindir")" head)
    fi
    emit_upstream "$localrev"
}

# ---------------------------------------------------------------------------
# preflight
#
# Seconds, never compiles. A GPU is not a hard requirement — sd.cpp builds and
# runs CPU-only — but unlike llama.cpp that is barely a usable configuration for
# diffusion, so it is said plainly instead of being left for someone to discover
# at a minute per image.
# ---------------------------------------------------------------------------
preflight() {
    local missing_names=() missing_pkgs=() notes=() cmds=()

    have git || { missing_names+=("git"); missing_pkgs+=("git"); }

    local cv; cv=$(cmake_version)
    if [ -z "$cv" ]; then
        missing_names+=("cmake >= $MIN_CMAKE")
        missing_pkgs+=("cmake")
    elif ! version_ge "$cv" "$MIN_CMAKE"; then
        missing_names+=("cmake >= $MIN_CMAKE (have $cv) — the floor is ggml's, not the root project's")
    fi

    if ! have c++ && ! have g++ && ! have clang++; then
        missing_names+=("a C++ compiler")
        missing_pkgs+=("build-essential")
    fi

    case "$(uname -s)" in
        Darwin)
            notes+=("Apple platform: builds with -DSD_METAL=ON — sd.cpp defaults EVERY backend off, so an unflagged build here would be CPU-only and look like a GPU one")
            # Upstream's own caveat, in docs/build.md. Worth repeating where
            # someone reads it before committing a machine to this, rather than
            # after measuring a disappointing number.
            notes+=("sd.cpp's docs call Metal 'highly inefficient' on very large matrices — the backend works, the throughput is upstream's known weak spot")
            ;;
        *)
            if have nvidia-smi; then
                local archs
                archs=$(nvidia-smi --query-gpu=compute_cap --format=csv,noheader,nounits 2>/dev/null \
                    | awk -F. 'NF==2 { printf "%d%d\n", $1, $2 }' | sort -un | paste -sd';')
                [ -n "$archs" ] && notes+=("CUDA build: targets arch(s) $archs — every card present contributes, so one binary serves a mixed box")
                if ! have nvcc && ! ls -d /usr/local/cuda-[0-9]*/bin/nvcc >/dev/null 2>&1; then
                    notes+=("nvidia-smi present but no nvcc found — the build would fall back to CPU-only")
                fi
            else
                notes+=("no NVIDIA GPU detected — builds CPU-only, which works but is minutes per image for diffusion, not seconds")
            fi
            ;;
    esac

    # The build fetches a submodule, so it needs the network for longer than the
    # initial clone. Said here because "cloned fine, died in cmake/ggml.cmake"
    # is otherwise a confusing way to learn it.
    notes+=("the build checks out the ggml submodule from github.com/leejet/ggml — an offline host clones and then fails at configure")

    # A note rather than a fix, because the recipe already passes
    # -DSD_SERVER_BUILD_FRONTEND=OFF. It is here for the person who runs cmake
    # by hand in that tree and cannot see why: the uninitialised
    # examples/server/frontend submodule still leaves an EMPTY DIRECTORY behind,
    # sd.cpp's cmake tests only that the directory EXISTS, finds pnpm, and runs
    # `pnpm -C <empty dir> install`, which fails the build.
    have pnpm && notes+=("pnpm is installed here: a hand-run cmake would try to build sd-server's web frontend out of the empty examples/server/frontend submodule and fail — this recipe passes -DSD_SERVER_BUILD_FRONTEND=OFF")

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

# sync_submodules brings ggml to the commit the superproject names.
#
# TWO SEPARATE FAILURES, and the second is the one that would waste an
# afternoon:
#
#  1. Without this the FIRST build dies. `git clone` leaves ggml/ empty and
#     cmake/ggml.cmake add_subdirectory()s it.
#  2. Without this at ALIGN TIME, a later build freezes the tree FOREVER. When
#     upstream bumps the ggml gitlink, `git reset --hard` moves the index while
#     the checked-out submodule stays put — which `git diff` reports as an
#     uncommitted change to a tracked path. align_tree refuses to move a tree in
#     that state (correctly: it cannot tell our stale submodule from a human's
#     edits), warns, and leaves it. The tool then silently stops taking upstream
#     while every verb keeps answering normally. Running this immediately after
#     each align is what guarantees the tree is clean when the NEXT one looks.
#
# examples/server/frontend and thirdparty/libwebp are deliberately NOT checked
# out — see the cmake arguments in build for what each one would cost.
sync_submodules() {
    local dir=$1
    (
    cd "$dir" || exit 1
    # sync first: the recorded URL can change under a tree that already has the
    # submodule registered, and update alone would keep fetching the old remote.
    git submodule sync --quiet -- ggml >&2 || exit 1
    say "  updating the ggml submodule"
    git submodule update --init --recursive -- ggml >&2 || exit 1
    say "  ggml at $(git -C ggml rev-parse --short HEAD)"
    )
}

# ---------------------------------------------------------------------------
# build
#
# Align → submodules → patch → configure → compile → install → stamp. Refuses on
# an adopted entry: TOOL_INSTALLED_AT means somebody else owns that tree, and
# the first thing a build does is `git clean -xdf`.
# ---------------------------------------------------------------------------
build() {
    local started=$SECONDS
    local src; src=$(tool_src_dir)
    local bindir; bindir=$(tool_bin_dir)

    if adopted; then
        die "refusing to build an adopted install ($TOOL_INSTALLED_AT) — corrallm does not write to a tree it does not own; drop installedAt to manage it here"
    fi

    unapply_patches "$src"
    local want; want=$(effective_ref)
    pinned && say "=== pinned at $want (tracking $TOOL_REF)"
    align_tree "$src" "$want" "$TOOL_URL" || die "could not align $src to $want"
    sync_submodules "$src" || die "could not check out the ggml submodule in $src — sd.cpp cannot configure without it"
    apply_patches "$src" || die "patch set does not apply to $(current_head "$src")"

    local head backend stamp_now
    head=$(current_head "$src")
    backend=$(backend_spec)
    stamp_now="head=$head patches=$(patch_set_hash) archs=$backend"

    if [ "${TOOL_FORCE:-0}" != "1" ] && [ -x "$bindir/$BIN_NAME" ] && [ "$(stamp_read "$bindir")" = "$stamp_now" ]; then
        say "up-to-date at $stamp_now; skipping build"
        printf '{"ok":true,"skipped":true,"id":%s,"head":%s,"stamp":%s,"seconds":%d,"error":""}\n' \
            "$(jstr "$(active_build)")" "$(jstr "$head")" "$(jstr "$stamp_now")" "$((SECONDS - started))"
        return 0
    fi

    say "=== building $src (backend=$backend)"
    (
    cd "$src" || exit 1
    # Leaves the submodule alone: ggml is a tracked path, so clean does not
    # descend into it, and sync_submodules has already put it where it belongs.
    git clean -xdf >&2

    # Pin ONE toolchain for both languages. sd.cpp vendors the same ggml whose
    # cmake/common.cmake derives its C warning flags from a single compiler id,
    # so a host whose `cc` and `c++` disagree gets one compiler's flags handed to
    # the other and dies early on ggml.c. llama.cpp.sh carries the long version
    # of this note and the box it was found on.
    if [ -z "${CC:-}" ] && [ -z "${CXX:-}" ] && have clang && have clang++; then
        CC=$(command -v clang); CXX=$(command -v clang++)
        export CC CXX
        say "  toolchain: $CC / $CXX (pinned; this host's cc and c++ disagree)"
    elif [ -n "${CC:-}" ] || [ -n "${CXX:-}" ]; then
        say "  toolchain: CC=${CC:-<default>} CXX=${CXX:-<default>} (from the environment)"
    fi

    local args=(
        -DCMAKE_BUILD_TYPE=Release
        # OFF because the frontend is a submodule this recipe does not check
        # out — and an uninitialised submodule still leaves an EMPTY DIRECTORY
        # on disk. sd.cpp's cmake tests only that the directory EXISTS, then
        # looks for pnpm and runs `pnpm -C <empty dir> install` as a build step.
        # The Mac has pnpm (it builds corrallm's own UI), so leaving this at its
        # ON default fails the build there. The cost is sd-server's bundled web
        # UI at /; every API route, /v1/images/generations included, is
        # unaffected, and the proxy is what corrallm actually talks to.
        -DSD_SERVER_BUILD_FRONTEND=OFF
        # Also unchecked-out submodules, and already the effective default when
        # they are absent. Stated anyway so the configuration does not depend on
        # which directories happen to be populated. The cost is narrow and
        # worth naming: PNG and JPEG are unaffected, and an OpenAI request
        # asking for `output_format: webp` gets a 500 rather than an image.
        -DSD_WEBP=OFF
        -DSD_WEBM=OFF
    )

    case "$backend" in
        cuda:*)
            nvidia-smi --query-gpu=name,compute_cap --format=csv,noheader 2>/dev/null | sed 's/^/  /' >&2
            args+=(-DSD_CUDA=ON -DCMAKE_CUDA_ARCHITECTURES="${backend#cuda:}")
            # llama.cpp.sh's other CUDA flags are deliberately NOT copied:
            # GGML_CUDA_FA_ALL_QUANTS and GGML_CUDA_FORCE_MMQ exist for a
            # quantised KV cache and a token-generation matmul, and a diffusion
            # pipeline has neither.
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
            ;;
        metal)
            # EXPLICIT, unlike llama.cpp. sd.cpp declares `option(SD_METAL ...
            # OFF)` and only forces GGML_METAL on when it is set; an unflagged
            # build on this Mac would inherit ggml's own Apple default, which is
            # a transitive default two projects deep rather than a decision.
            # Saying it here also puts `metal` in the stamp, so the CPU build
            # that came before it cannot satisfy the rebuild check.
            say "  Apple platform: -DSD_METAL=ON"
            args+=(-DSD_METAL=ON)
            ;;
        *)
            say "  no GPU backend selected; building CPU-only (supported, and slow for diffusion)"
            ;;
    esac

    cmake -B build "${args[@]}" >&2 || exit 1
    cmake --build build --config Release --parallel "$(cpu_count)" >&2 || exit 1
    ) || die "build failed; see log"

    local id; id=$(build_id "$head")
    # CMAKE_RUNTIME_OUTPUT_DIRECTORY is build/bin, and it holds both sd-server
    # and sd-cli. The CLI travels with the server on purpose: it is the only way
    # to test a checkpoint without a running server, and it costs nothing.
    install_build "$src/build/bin" "$id"
    stamp_write "$(builds_dir)/$id" "$stamp_now"
    prune_builds

    # Trimmed at the comma exactly as probe trims it. Reporting the whole
    # remainder here would make `tools build` and `tools list` name the same
    # binary two different ways, which reads as two builds.
    local version=""
    [ -x "$bindir/$BIN_NAME" ] && version=$("$bindir/$BIN_NAME" --version 2>&1 \
        | sed -n 's/^stable-diffusion\.cpp version \([^,]*\).*/\1/p' | head -1)

    printf '{"ok":true,"skipped":false,"id":%s,"head":%s,"version":%s,"stamp":%s,"seconds":%d,"error":""}\n' \
        "$(jstr "$id")" "$(jstr "$head")" "$(jstr "$version")" "$(jstr "$stamp_now")" "$((SECONDS - started))"
}

case "${1:-}" in
    probe)        require_tool_root; probe ;;
    builds)       require_tool_root; builds_verb ;;
    activate)     require_tool_root; activate_verb ;;
    upstream)     require_tool_root; require_env TOOL_URL TOOL_REF; upstream ;;
    build)        require_tool_root; require_env TOOL_NAME TOOL_URL TOOL_REF; build ;;
    preflight)    preflight ;;
    install-deps) install_deps ;;
    *)            die "unknown verb: ${1:-<none>}" ;;
esac
