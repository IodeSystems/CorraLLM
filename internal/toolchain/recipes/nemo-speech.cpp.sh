#!/usr/bin/env bash
# NeMo-Speech.cpp recipe — probe | upstream | preflight | install-deps | build
#
# https://github.com/NVIDIA/NeMo-Speech.cpp — NVIDIA's ggml port of the NeMo
# speech stack. corrallm wants one binary out of it, `nemo-speech serve`, whose
# /v1/audio/transcriptions is an OpenAI-compatible subset: the corral layer
# remaps it rather than translating it.
#
# It is the same ggml/GGUF family as llama.cpp and stable-diffusion.cpp, and
# stable-diffusion.cpp.sh is the recipe it is closest to — submodules, a backend
# that is off unless asked for, and a version banner on the wrong stream. FIVE
# THINGS MAKE IT UNLIKE THAT ONE, and every one is a build that fails or an
# answer that lies if the recipe pretends otherwise:
#
#  1. ITS ggml IS PATCHED IN PLACE BY THE TREE ITSELF. 23 patches in
#     ggml-patches/ are applied to the ggml submodule's WORKTREE by
#     scripts/apply-ggml-patches.sh. A dirty submodule worktree makes the
#     SUPERPROJECT's `git diff --quiet` exit 1 (verified), which is exactly what
#     common.sh's align_tree refuses to move — so from the second build onward
#     the tree would freeze and silently stop taking upstream. See
#     reset_vendored_ggml, which runs BEFORE align_tree for that reason, where
#     sd.cpp's sync_submodules runs only after.
#  2. IT NEEDS SASS, NOT PTX. See sass_only, and the CUDA_ARCHS escape there.
#  3. ITS VERSION BANNER CARRIES NO COMMIT. `nemo-speech --version` prints
#     `nemo-speech 0.1.0` to STDOUT — a third answer to the question llama.cpp
#     (stderr, with a commit) and sd.cpp (stdout, with a commit) give
#     differently. 0.1.0 is read out of the VERSION file at configure time and
#     has not moved since the repo opened, so it identifies nothing. See probe.
#  4. IT HAS A DEPENDENCY THAT IS NOT A SUBMODULE. SentencePiece is a hard
#     requirement of the ASR library and is not vendored. See build_sentencepiece.
#  5. scripts/configure.sh CANNOT BE USED. It demands the llama.cpp submodule
#     for every ASR preset (scripts/configure.sh:142-144) although ASR needs it
#     only for miniaudio, and only with NEMO_SPEECH_BUILD_MIC_CAPTURE=ON. That
#     submodule is ~400 MB of clone for a header this build does not compile.
#     The recipe invokes `cmake --preset` directly and does configure.sh's two
#     useful jobs — the submodules and the ggml series — itself.

DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=./common.sh
source "$DIR/common.sh"

BIN_NAME=${TOOL_BIN:-nemo-speech}

# CMakePresets.json opens `cmakeMinimumRequired: 3.26.0`, and the build is
# driven by `cmake --preset`, so the floor is the presets file's rather than the
# root project's. Named here so the answer is "cmake is too old" instead of
# "unknown preset cuda-asr", which reads like a typo in this script.
MIN_CMAKE=3.26

version_ge() { [ "$(printf '%s\n%s\n' "$2" "$1" | sort -V | head -1)" = "$2" ]; }

cmake_version() { have cmake && cmake --version 2>/dev/null | head -1 | awk '{print $3}'; }

# ---------------------------------------------------------------------------
# What this host builds for
#
# The preset IS the backend here — build/<preset>/ is where the output lands
# (CMakePresets.json sets binaryDir to ${sourceDir}/build/${presetName}) — so
# backend and preset are resolved together and both go in the stamp, for the
# reason sd.cpp's backend_spec gives: one commit and one patch set produce
# different binaries per backend, and a stamp that could not tell them apart
# would skip the build that turns the GPU on.
#
# Vulkan is deliberately absent although vulkan-asr exists. Nothing here has
# ever run it, and offering a backend this recipe cannot claim to have built is
# worse than making somebody ask for it.
# ---------------------------------------------------------------------------

# sass_only turns an arch list into a SASS-ONLY one: 86;120 -> 86-real;120-real.
#
# THIS IS NOT COSMETIC AND IT IS NOT COPIED FROM THE OTHER RECIPES, which pass
# the bare list and are right to. box1's driver is CUDA 13.2 while its only
# toolkit is 13.3, so any PTX this build emits is newer than the driver can
# consume: the first kernel that JITs dies with "the provided PTX was compiled
# with an unsupported toolchain". llama.cpp builds `86;120` on this same box and
# never trips it, because every card present has SASS and nothing has to JIT —
# NeMo does trip it, so `-real` (emit SASS, no PTX, nothing to JIT) removes the
# question rather than relying on the answer.
#
# WHAT IT COSTS, plainly: a SASS-only binary will not run on a GPU whose arch is
# not in the list, where a PTX-carrying one would JIT and work. That fallback
# was never the mechanism corrallm relies on — the arch list is DETECTED from
# the cards in the box and is in the stamp, so adding a card forces a rebuild
# either way (cuda_arch_spec's comment in common.sh makes the same argument).
#
# CUDA_ARCHS is passed through VERBATIM and is not touched: it is the override
# for somebody who knows their driver and toolkit agree and wants the PTX back.
sass_only() {
    if [ -n "${CUDA_ARCHS:-}" ]; then printf '%s' "$CUDA_ARCHS"; return; fi
    printf '%s' "$1" | tr ';' '\n' | sed 's/$/-real/' | paste -sd';'
}

backend_spec() {
    if have nvidia-smi && [ "$(cuda_arch_spec)" != "cpu" ]; then
        printf 'cuda:%s' "$(sass_only "$(cuda_arch_spec)")"
    elif [ "$(uname -s)" = "Darwin" ]; then
        printf 'metal'
    else
        printf 'cpu'
    fi
}

# preset_for names the CMake preset for a backend. ASR, not *-server: the
# cuda-server preset turns NMT and TTS on, and NMT is the one feature that
# genuinely needs the llama.cpp submodule (scripts/configure.sh:141-142). HTTP
# is added as a flag instead — see build.
preset_for() {
    case "$1" in
        cuda:*) printf 'cuda-asr' ;;
        metal)  printf 'metal-asr' ;;
        *)      printf 'cpu-asr' ;;
    esac
}

# ---------------------------------------------------------------------------
# probe
#
# `nemo-speech --version` prints to STDOUT and exits 0:
#
#   nemo-speech 0.1.0
#
# and that is ALL it prints. There is no commit in it: CMakeLists.txt:12-22
# parses the VERSION file and bakes NEMO_SPEECH_VERSION_STR, with no `git
# describe` anywhere, so a tree at 97a15af and a tree six months of commits
# later both answer `0.1.0`. Upstream has published no release past v0.1.0.
#
# So the banner proves the binary RUNS and the STAMP is the only thing that says
# WHICH ONE — the situation ninfer.sh is in, arrived at from the other
# direction. The two halves are reported from the two sources and `source` says
# which one the version came from, rather than presenting a constant as if it
# had been measured.
#
# CONSEQUENCE WORTH KNOWING: an ADOPTED nemo-speech (installedAt) has no stamp
# and cannot be told it is behind. sd.cpp's binary reports its own commit and so
# gets drift on day one; this one cannot, and `upstream` says so by answering
# with an empty local rather than inventing one.
#
# Both streams are read and the line is matched by its PREFIX, for sd.cpp's
# reason: merging costs nothing while the anchored match keeps a stray log line
# from being mistaken for a version.
# ---------------------------------------------------------------------------
probe() {
    local bindir; bindir=$(tool_bin_dir)
    local path="$bindir/$BIN_NAME"

    if [ ! -x "$path" ]; then
        printf '{"present":false,"path":%s,"version":"","commit":"","source":"","stamp":"","error":""}\n' \
            "$(jstr "$path")"
        return 0
    fi

    local release commit stamp version source
    release=$("$path" --version 2>&1 | sed -n 's/^nemo-speech //p' | head -1)
    stamp=$(stamp_read "$bindir")
    commit=$(stamp_field "$stamp" head)

    if [ -n "$release" ]; then
        version=$release
        source=binary
    elif [ -n "$commit" ]; then
        # The binary would not answer — a broken install, or a future build that
        # moved the banner. The stamp still knows what was compiled.
        version=$commit
        source=stamp
    fi

    printf '{"present":true,"path":%s,"version":%s,"commit":%s,"source":%s,"stamp":%s,"error":""}\n' \
        "$(jstr "$path")" "$(jstr "$version")" "$(jstr "$commit")" \
        "$(jstr "$source")" "$(jstr "$stamp")"
}

# upstream reports drift from the stamp alone. Unlike sd.cpp there is no second
# path through the binary's banner, because the banner carries no commit (see
# probe) — an adopted install reports an empty local, which emit_upstream
# renders as "not behind" rather than as a guess.
upstream() {
    emit_upstream "$(stamp_field "$(stamp_read "$(tool_bin_dir)")" head)"
}

# ---------------------------------------------------------------------------
# preflight
#
# Seconds, never compiles.
# ---------------------------------------------------------------------------

# ninja_pkg is the package name for THIS host's manager. ninja is not optional
# here — every preset in CMakePresets.json sets "generator": "Ninja" and a
# preset's generator cannot be overridden on the cmake command line — and the
# three managers do not agree on what to call it, so a single name in
# missing_pkgs would print an install command that fails on half of them.
# compiler_family answers clang / gcc / other for one driver.
compiler_family() {
    local v; v=$("$1" --version 2>/dev/null | head -1)
    case "$v" in
        *clang*|*Clang*) printf 'clang' ;;
        # `cc --version` on Debian/Ubuntu names neither gcc nor GCC; the
        # "Free Software Foundation" line two lines down is the reliable tell,
        # so the version banner is read as a whole rather than by its first line.
        *) if "$1" --version 2>/dev/null | grep -qi 'free software foundation'; then
               printf 'gcc'
           else
               printf 'other'
           fi ;;
    esac
}

ninja_pkg() {
    case "$(pkg_manager)" in
        apt|dnf) printf 'ninja-build' ;;
        *)       printf 'ninja' ;;
    esac
}

preflight() {
    local missing_names=() missing_pkgs=() notes=() cmds=()

    have git || { missing_names+=("git"); missing_pkgs+=("git"); }
    have curl || notes+=("curl is not on PATH — the binary builds, but \`nemo-speech pull\` uses curl to fetch models and cannot run without it")

    local cv; cv=$(cmake_version)
    if [ -z "$cv" ]; then
        missing_names+=("cmake >= $MIN_CMAKE")
        missing_pkgs+=("cmake")
    elif ! version_ge "$cv" "$MIN_CMAKE"; then
        missing_names+=("cmake >= $MIN_CMAKE (have $cv) — the floor is CMakePresets.json's, and the build is driven by --preset")
    fi

    if ! have ninja && ! have ninja-build; then
        missing_names+=("ninja — every preset sets generator Ninja and a preset's generator cannot be overridden")
        missing_pkgs+=("$(ninja_pkg)")
    fi

    if ! have c++ && ! have g++ && ! have clang++; then
        missing_names+=("a C++ compiler")
        missing_pkgs+=("build-essential")
    fi

    # The disagreeing-compiler trap, reported BEFORE the build rather than as a
    # wall of warnings 2% in. See the toolchain block in build for the fix.
    #
    # Compared by FAMILY, not by matching two banners against each other:
    # box1's `cc` introduces itself as `cc (Ubuntu 13.3.0-...) 13.3.0` and never
    # says the word "gcc", so looking for both names in one string finds
    # nothing on the very host this note exists for.
    if have cc && have c++; then
        local ccfam cxxfam
        ccfam=$(compiler_family cc)
        cxxfam=$(compiler_family c++)
        if [ "$ccfam" != "$cxxfam" ]; then
            notes+=("this host's \`cc\` is $ccfam and its \`c++\` is $cxxfam — the recipe pins ONE compiler for both languages, because ggml derives its C warning flags from the C++ compiler id and applies them to C")
        fi
    fi

    case "$(uname -s)" in
        Darwin)
            notes+=("Apple platform: builds the metal-asr preset. NOTE that preset inherits NEMO_SPEECH_GGML_PATCHED=OFF from cpu-asr, so the ggml series is applied (as scripts/configure.sh does for metal-*) but the fused CUDA/Metal ops stay unwired — this is upstream's own arrangement, not a choice made here")
            ;;
        *)
            if have nvidia-smi; then
                local archs; archs=$(cuda_arch_spec)
                if [ "$archs" != "cpu" ]; then
                    notes+=("CUDA build: targets arch(s) $(sass_only "$archs") — SASS only, no PTX. A card whose arch is not in that list cannot run this binary at all; set CUDA_ARCHS to override verbatim")
                fi
                if ! have nvcc && ! ls -d /usr/local/cuda-[0-9]*/bin/nvcc >/dev/null 2>&1; then
                    notes+=("nvidia-smi present but no nvcc found — the build would fall back to the cpu-asr preset")
                fi
                # Not something this recipe can fix, and the single most
                # expensive thing to learn at runtime instead of here.
                notes+=("naming a GPU: CUDA orders devices FASTEST_FIRST, which is NOT nvidia-smi's order. A \`--device cuda:0\` that means a particular card is wrong on a mixed box; set CUDA_VISIBLE_DEVICES to that card's UUID in the model's cmd, as every other local model on this box does")
            else
                notes+=("no NVIDIA GPU detected — builds the cpu-asr preset, which is supported and has never been measured here")
            fi
            ;;
    esac

    # Three separate network needs, said together because "cloned fine, died in
    # cmake" is a confusing way to learn any of them.
    notes+=("the build fetches two submodules (ggml, third_party/cpp-httplib) and clones SentencePiece — an offline host clones the repo and then fails at configure")
    notes+=("the llama.cpp submodule is NOT fetched: ASR needs it only for vendor/miniaudio, and only with NEMO_SPEECH_BUILD_MIC_CAPTURE=ON, which this recipe leaves off")

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

# ---------------------------------------------------------------------------
# reset_vendored_ggml — THE ONE THING THAT WOULD OTHERWISE FREEZE THE TREE
#
# scripts/apply-ggml-patches.sh edits the ggml submodule's WORKTREE. Three
# measured facts follow from that, and each drives one line below:
#
#  1. A dirty submodule worktree makes the SUPERPROJECT's `git diff --quiet`
#     exit 1. align_tree refuses to move a tree in that state — correctly, since
#     it cannot tell our patched ggml from a human's edits — so it would warn
#     once and then leave the checkout at the patched commit forever while every
#     verb kept answering normally. This is the freeze sd.cpp's sync_submodules
#     warns about, arriving by a different route and GUARANTEED rather than
#     occasional: it happens on the second build, every time.
#  2. `git submodule update --force` alone clears the tracked modifications
#     (diff --quiet goes back to 0) but leaves UNTRACKED files the series
#     created. apply-ggml-patches.sh compares the whole ggml worktree against
#     the pinned commit plus the full series and refuses a tree that is
#     partially patched or carrying leftovers, so the leftovers have to go too.
#  3. A superproject `git clean -xdf` does NOT descend into a submodule, so
#     build's clean cannot do this job.
#
# Which is why this is a reset AND a clean, and why it runs before align_tree
# and not after it.
# ---------------------------------------------------------------------------
reset_vendored_ggml() {
    local dir=$1
    [ -d "$dir/ggml/.git" ] || [ -f "$dir/ggml/.git" ] || return 0
    say "  restoring the ggml submodule to its pinned commit"
    (cd "$dir/ggml" && git reset --hard >/dev/null 2>&1 && git clean -xdfq >/dev/null 2>&1) || true
}

# sync_submodules checks out exactly the two submodules this build compiles.
#
# ggml is the backend. cpp-httplib is the HTTP server and CMakeLists.txt:383
# fails the configure by name without it. The other six in .gitmodules
# (llama.cpp, riva-common, flashlight-text, kenlm, open_jtalk, cppjieba) belong
# to features this recipe leaves off, and llama.cpp alone is the larger clone.
sync_submodules() {
    local dir=$1
    (
    cd "$dir" || exit 1
    git submodule sync --quiet -- ggml third_party/cpp-httplib >&2 || exit 1
    say "  updating the ggml and cpp-httplib submodules"
    git submodule update --init --force -- ggml third_party/cpp-httplib >&2 || exit 1
    say "  ggml at $(git -C ggml rev-parse --short HEAD)"
    )
}

# apply_ggml_series runs the tree's OWN patch series, which is not corrallm's.
#
# Two patch sets are in play and confusing them wastes a build. common.sh's
# apply_patches carries CORRALLM's patches, from <prefix>/patches, against the
# superproject; this carries UPSTREAM's 23, from ggml-patches/, against the
# vendored ggml. They are ordered corrallm-first so that a corrallm patch is
# able to edit ggml-patches/ itself — which is the only reason a corrallm patch
# would exist for this tool.
#
# Applied for cuda-* and metal-* only, which is the condition
# scripts/configure.sh:170-171 uses. The cpu-asr preset sets
# NEMO_SPEECH_GGML_PATCHED=OFF and wants a stock ggml.
apply_ggml_series() {
    local dir=$1 preset=$2
    case "$preset" in
        cuda-*|metal-*) ;;
        *) say "  $preset wants stock ggml (NEMO_SPEECH_GGML_PATCHED=OFF); skipping the series"; return 0 ;;
    esac
    say "  applying the in-tree ggml series ($(ls -1 "$dir"/ggml-patches/*.patch 2>/dev/null | wc -l | tr -d ' ') patches)"
    (cd "$dir" && ./scripts/apply-ggml-patches.sh >&2)
}

# build_sentencepiece provides the ASR library's one non-vendored dependency.
#
# src/asr/CMakeLists.txt:71-103 takes a private static archive if it finds one,
# then vcpkg's target, then a system libsentencepiece REQUIRED — so without one
# of the three the configure dies, and upstream's answer
# (scripts/build_sentencepiece_static.sh) is a static build whose bundled
# protobuf symbols stay out of the .so's ABI.
#
# BUILT OUTSIDE THE CHECKOUT, which is the one change made to upstream's script:
# its default PREFIX is <src>/.deps, and build's `git clean -xdf` deletes that
# (untracked, and -x takes ignored files too), so every build would re-clone and
# re-compile SentencePiece. Under <prefix>/deps it survives the clean, survives
# align_tree's reset, and is found because NEMO_SPEECH_DEPENDENCY_PREFIX is
# passed to point at it (CMakeLists.txt:118).
build_sentencepiece() {
    local dir=$1
    local deps; deps="$(tool_prefix)/deps"
    if [ -f "$deps/sentencepiece/lib/libsentencepiece.a" ] && \
       [ -f "$deps/sentencepiece/include/sentencepiece_processor.h" ]; then
        say "  SentencePiece already built at $deps/sentencepiece"
        return 0
    fi
    say "=== building SentencePiece (static, once per prefix) -> $deps/sentencepiece"
    mkdir -p "$deps"
    (cd "$dir" && PREFIX="$deps/sentencepiece" JOBS="$(cpu_count)" \
        ./scripts/build_sentencepiece_static.sh "$deps/sentencepiece-build" >&2)
}

# ---------------------------------------------------------------------------
# build
#
# ggml reset → align → submodules → corrallm patches → ggml series →
# SentencePiece → configure → compile → install → stamp. Refuses on an adopted
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

    local backend preset
    backend=$(backend_spec)
    preset=$(preset_for "$backend")

    unapply_patches "$src"
    reset_vendored_ggml "$src"
    local want; want=$(effective_ref)
    pinned && say "=== pinned at $want (tracking $TOOL_REF)"
    align_tree "$src" "$want" "$TOOL_URL" || die "could not align $src to $want"
    sync_submodules "$src" || die "could not check out ggml and cpp-httplib in $src — NeMo-Speech.cpp cannot configure without them"
    apply_patches "$src" || die "patch set does not apply to $(current_head "$src")"
    apply_ggml_series "$src" "$preset" || die "the in-tree ggml series does not apply — the submodule is not at its pinned commit, or upstream moved a patch"

    local head stamp_now
    head=$(current_head "$src")
    # The 23 ggml patches are TRACKED CONTENT at this commit, so `head` already
    # covers them and they need no field of their own; `patches` remains
    # corrallm's own set, as in every other recipe.
    stamp_now="head=$head patches=$(patch_set_hash) archs=$backend"

    if [ "${TOOL_FORCE:-0}" != "1" ] && [ -x "$bindir/$BIN_NAME" ] && [ "$(stamp_read "$bindir")" = "$stamp_now" ]; then
        say "up-to-date at $stamp_now; skipping build"
        printf '{"ok":true,"skipped":true,"id":%s,"head":%s,"stamp":%s,"seconds":%d,"error":""}\n' \
            "$(jstr "$(active_build)")" "$(jstr "$head")" "$(jstr "$stamp_now")" "$((SECONDS - started))"
        return 0
    fi

    build_sentencepiece "$src" || die "could not build SentencePiece; src/asr/CMakeLists.txt requires it and nothing here vendors it"

    say "=== building $src (backend=$backend preset=$preset)"
    (
    cd "$src" || exit 1
    # Leaves the submodules alone — they are tracked paths, so clean does not
    # descend into them (verified), and the two steps above have already put
    # them where they belong. It DOES delete build/, which is required rather
    # than merely tidy: a CMake cache stores absolute source and build paths, so
    # a tree that was ever copied cannot reconfigure over its old one.
    git clean -xdf >&2

    # PIN ONE TOOLCHAIN FOR BOTH LANGUAGES — the same trap llama.cpp.sh and
    # sd.cpp.sh carry, with the opposite answer. ggml's cmake/common.cmake
    # derives its C warning flags from CMAKE_CXX_COMPILER_ID alone and applies
    # them to C targets, so a host whose `cc` and `c++` disagree hands one
    # compiler's flags to the other and dies on ggml.c.
    #
    # GCC, where the other two recipes pick clang. Not a preference: GNU 13 is
    # the toolchain this build has actually been performed with, twice, and
    # clang has never compiled this tree here. Picking the measured one is worth
    # more than matching the sibling recipes.
    if [ -z "${CC:-}" ] && [ -z "${CXX:-}" ] && have gcc && have g++; then
        CC=$(command -v gcc); CXX=$(command -v g++)
        export CC CXX
        say "  toolchain: $CC / $CXX (pinned for both languages)"
    elif [ -z "${CC:-}" ] && [ -z "${CXX:-}" ] && have clang && have clang++; then
        CC=$(command -v clang); CXX=$(command -v clang++)
        export CC CXX
        say "  toolchain: $CC / $CXX (pinned; no gcc/g++ on this host — UNTESTED for this tool)"
    elif [ -n "${CC:-}" ] || [ -n "${CXX:-}" ]; then
        say "  toolchain: CC=${CC:-<default>} CXX=${CXX:-<default>} (from the environment)"
    fi

    local args=(
        # The ONE addition to the preset, and the reason the *-server presets
        # are not used: those turn NMT and TTS on with it, and NMT is what
        # actually needs the llama.cpp submodule.
        -DNEMO_SPEECH_BUILD_HTTP=ON
        # Explicit, although cuda-asr already sets them: the flags say what this
        # binary is for, and the llama.cpp submodule is not checked out, so a
        # preset that ever flipped one of these would fail the configure with a
        # missing-submodule error rather than by naming the feature.
        #
        # MIC_CAPTURE is the whole reason llama.cpp is skippable: it is the only
        # ASR-side consumer of llama.cpp/vendor/miniaudio/miniaudio.h. The cost
        # is that `nemo-speech` cannot record from a local microphone, which is
        # not something a server behind corrallm can do anyway.
        #
        # NMT off costs /v1/audio/translations: the route answers 501 and
        # corrallm forwards it. Turning it on needs the llama.cpp submodule AND
        # a translation model.
        -DNEMO_SPEECH_BUILD_TTS=OFF
        -DNEMO_SPEECH_BUILD_NMT=OFF
        -DNEMO_SPEECH_BUILD_MIC_CAPTURE=OFF
        -DNEMO_SPEECH_DEPENDENCY_PREFIX="$(tool_prefix)/deps"
    )

    case "$backend" in
        cuda:*)
            nvidia-smi --query-gpu=name,compute_cap --format=csv,noheader 2>/dev/null | sed 's/^/  /' >&2
            args+=(-DCMAKE_CUDA_ARCHITECTURES="${backend#cuda:}")
            local cuda_home; cuda_home=$(cuda_toolkit_home)
            if [ -n "$cuda_home" ]; then
                export CUDACXX="$cuda_home/bin/nvcc"
                export PATH="$cuda_home/bin:$PATH"
                export LD_LIBRARY_PATH="$cuda_home/lib64:${LD_LIBRARY_PATH:-}"
                say "  using $cuda_home/bin/nvcc ($("$cuda_home/bin/nvcc" --version | sed -n 's/.*release \([0-9.]*\).*/\1/p' | head -1))"
                args+=(-DCMAKE_CUDA_COMPILER="$cuda_home/bin/nvcc")
            fi
            ;;
        metal) say "  Apple platform: the metal-asr preset sets GGML_METAL=ON" ;;
        *)     say "  no GPU backend selected; building cpu-asr (supported, never measured here)" ;;
    esac

    cmake --preset "$preset" "${args[@]}" >&2 || exit 1
    cmake --build "build/$preset" --parallel "$(cpu_count)" >&2 || exit 1
    ) || die "build failed; see log"

    local id; id=$(build_id "$head")
    # build/<preset>/bin holds nemo-speech beside libggml*.so and
    # libnemo_speech_asr*.so, and the binary's RUNPATH is $ORIGIN — so the whole
    # directory travels together or the binary does not start. install_build
    # copies the directory, which is what makes that work.
    install_build "$src/build/$preset/bin" "$id"
    stamp_write "$(builds_dir)/$id" "$stamp_now"
    prune_builds

    local version=""
    [ -x "$bindir/$BIN_NAME" ] && version=$("$bindir/$BIN_NAME" --version 2>&1 | sed -n 's/^nemo-speech //p' | head -1)

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
