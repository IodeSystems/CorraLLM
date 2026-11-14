#!/usr/bin/env bash
# Build COLMAP from source with CUDA into $COLMAP_WORK/install (idempotent).
#
#   COLMAP_WORK     work dir: src/ (clone), build/, install/   (required)
#   COLMAP_SRC      existing checkout to use instead of cloning into $COLMAP_WORK/src
#   COLMAP_VERSION  git tag (default 4.2.1)
#   CUDA_HOME       toolkit (default /usr/local/cuda); its nvcc AND its cudart are used
#   CUDA_ARCH       CMAKE_CUDA_ARCHITECTURES (default 86 = RTX 3080)
#   JOBS            parallel compile jobs (default 8)
#
# No sudo: needs the apt dev packages already installed (boost, eigen3, ceres,
# openimageio, cgal, glog, gflags, metis, suitesparse, glew, ...). GUI and ONNX
# off. Skips the build when install/.stamp matches version, arch and nvcc.
# The install dir is replaced atomically (install.new → install).
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
: "${COLMAP_WORK:?set COLMAP_WORK}"
COLMAP_VERSION=${COLMAP_VERSION:-4.2.1}
CUDA_HOME=${CUDA_HOME:-/usr/local/cuda}
CUDA_ARCH=${CUDA_ARCH:-86}
JOBS=${JOBS:-8}
nvcc="$CUDA_HOME/bin/nvcc"
[ -x "$nvcc" ] || { echo "no nvcc at $nvcc (set CUDA_HOME)" >&2; exit 1; }
cudart_dir="$CUDA_HOME/targets/x86_64-linux/lib"
[ -d "$cudart_dir" ] || cudart_dir="$CUDA_HOME/lib64"

mkdir -p "$COLMAP_WORK"
install="$COLMAP_WORK/install"
want="colmap=$COLMAP_VERSION arch=$CUDA_ARCH nvcc=$("$nvcc" --version | tail -1)"
if [ -x "$install/bin/colmap" ] && [ "$(cat "$install/.stamp" 2>/dev/null)" = "$want" ]; then
    echo "colmap: up to date ($want)"
    exit 0
fi

src=${COLMAP_SRC:-$COLMAP_WORK/src}
if [ ! -d "$src/.git" ]; then
    git clone --depth 1 --branch "$COLMAP_VERSION" https://github.com/colmap/colmap.git "$src"
fi
have=$(git -C "$src" describe --tags --exact-match 2>/dev/null || true)
[ "$have" = "$COLMAP_VERSION" ] || { echo "colmap: $src is at '${have:-?}', not $COLMAP_VERSION" >&2; exit 1; }

oiio=$("$here/oiio-cmake-fix.sh" "$COLMAP_WORK/oiio-cmake")
gen=()
command -v ninja >/dev/null && gen=(-G Ninja)
build="$COLMAP_WORK/build"
start=$(date +%s)
# gcc, not the system c++ (clang++ on box1); CUDAToolkit_ROOT so only this
# toolkit's cudart is linked (corrallm 263dec5: FindCUDAToolkit otherwise took
# the distro libcudart 12 next to nvcc 13).
cmake -S "$src" -B "$build" "${gen[@]}" \
    -DCMAKE_BUILD_TYPE=Release \
    -DCMAKE_C_COMPILER=gcc -DCMAKE_CXX_COMPILER=g++ -DCMAKE_CUDA_HOST_COMPILER=g++ \
    -DCMAKE_CUDA_COMPILER="$nvcc" -DCUDAToolkit_ROOT="$CUDA_HOME" \
    -DCMAKE_CUDA_ARCHITECTURES="$CUDA_ARCH" \
    -DGUI_ENABLED=OFF -DONNX_ENABLED=OFF -DTESTS_ENABLED=OFF \
    -DOpenImageIO_DIR="$oiio" \
    -DCMAKE_INSTALL_PREFIX="$install.new"
cmake --build "$build" --parallel "$JOBS"
rm -rf "$install.new"
cmake --install "$build" >/dev/null

bin="$install.new/bin/colmap"
# Exactly one CUDA runtime, the toolkit's own; ship it beside the binary
# (RUNPATH is $ORIGIN/../lib) so the install does not depend on ldconfig.
cudarts=$(ldd "$bin" | awk '/libcudart\.so/ {print $1}' | sort -u)
[ "$(echo "$cudarts" | grep -c .)" = 1 ] || { echo "colmap: links $(echo $cudarts) — want one cudart" >&2; exit 1; }
mkdir -p "$install.new/lib"
cp -P "$cudart_dir"/libcudart.so.[0-9]* "$install.new/lib/"
"$bin" -h | head -1 | grep -q "with CUDA" || { echo "colmap: built without CUDA" >&2; exit 1; }
echo "$want" > "$install.new/.stamp"
rm -rf "$install.old"
[ -e "$install" ] && mv "$install" "$install.old"
mv "$install.new" "$install"
rm -rf "$install.old"
echo "colmap: built in $(( $(date +%s) - start )) s: $("$install/bin/colmap" -h | head -1)"
