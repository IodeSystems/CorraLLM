#!/usr/bin/env bash
# Copy the distro OpenImageIO CMake config to $1 without the /usr/include/opencv4
# include path. Ubuntu's libopenimageio-dev exports that path but does not depend
# on libopencv-dev, so CMake refuses the imported target when OpenCV headers are
# absent. COLMAP does not use OIIO's OpenCV interop. Prints the new dir.
set -euo pipefail
src=${OIIO_CMAKE_DIR:-/usr/lib/x86_64-linux-gnu/cmake/OpenImageIO}
dst=$1
[ -f "$src/OpenImageIOTargets.cmake" ] || { echo "no OpenImageIO CMake config at $src" >&2; exit 1; }
mkdir -p "$dst"
cp "$src"/*.cmake "$dst"/
if [ -d /usr/include/opencv4 ]; then echo "$dst"; exit 0; fi
# Resolve prefixes against the original location, not the copy.
sed -i -e "1,107s#\${CMAKE_CURRENT_LIST_DIR}#$src#g" "$dst/OpenImageIOConfig.cmake"
sed -i -e "50s#\${CMAKE_CURRENT_LIST_FILE}#$src/OpenImageIOTargets.cmake#" \
       -e 's#;/usr/include/opencv4##' \
       -e '/INTERFACE_SYSTEM_INCLUDE_DIRECTORIES "\/usr\/include\/opencv4"/d' \
       "$dst/OpenImageIOTargets.cmake"
echo "$dst"
