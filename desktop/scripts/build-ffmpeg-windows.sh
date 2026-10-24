#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
#
# Builds the minimal LGPL ffmpeg/ffprobe for Windows x64 (SPEC-v3 V7, lead
# ruling of gate fix 2) by cross compiling on Linux with mingw-w64. Every
# input is pinned in packaging/third-party.json (ffmpeg["windows-x64"]):
# the FFmpeg 9.0.2 tarball and the libraries (zlib, nv-codec-headers,
# OpenH264, libvpl), each by URL and SHA-256. Nothing else is linked in:
# --disable-autodetect, no GPL, no non-free, no version3.
#
#   CI:      ubuntu runner:  sudo bash scripts/build-ffmpeg-windows.sh --install-deps
#   locally: Docker (scripts/stage.js does this when the output is missing):
#            docker run --rm -v "<desktop>:/src" -w /src -e FFWIN_WORK=/tmp/ffwin \
#              <docker_image from the manifest> bash scripts/build-ffmpeg-windows.sh --install-deps
#
# Output: desktop/.cache/ffmpeg-windows-<version>/
#   bin/ffmpeg.exe, bin/ffprobe.exe, bin/*.dll, COPYING.LGPLv2.1, LICENSE.md,
#   thirdparty/<library licenses>, BUILDINFO.txt (toolchain, configure line)
# The import table of every executable and DLL is checked against
# allowed_imports (Windows system libraries and FFmpeg's own DLLs only).
set -euo pipefail

cd "$(dirname "$0")/.."
desktop=$PWD
manifest="$desktop/packaging/third-party.json"

if [ "${1:-}" = "--install-deps" ]; then
  pkgs=$(python3 -c 'import json,sys; print(" ".join(json.load(open(sys.argv[1]))["ffmpeg"]["windows-x64"]["apt_packages"]))' "$manifest" 2>/dev/null || true)
  if [ -z "$pkgs" ]; then
    # python3 is itself one of the packages: bootstrap it first.
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -qq && apt-get install -y -qq --no-install-recommends python3 >/dev/null
    pkgs=$(python3 -c 'import json,sys; print(" ".join(json.load(open(sys.argv[1]))["ffmpeg"]["windows-x64"]["apt_packages"]))' "$manifest")
  fi
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq
  # shellcheck disable=SC2086
  apt-get install -y -qq --no-install-recommends $pkgs >/dev/null
fi

m() { python3 - "$manifest" "$@" <<'PY'
import json, sys
spec = json.load(open(sys.argv[1]))["ffmpeg"]
w = spec["windows-x64"]
expr = sys.argv[2]
print(eval(expr, {"spec": spec, "w": w}))
PY
}

version=$(m 'spec["version"]')
out="${FFWIN_OUT:-$desktop/.cache/ffmpeg-windows-$version}"
work="${FFWIN_WORK:-$desktop/.cache/ffmpeg-windows-work}"
dl="${FFWIN_DOWNLOADS:-$desktop/.cache/ffmpeg-windows-downloads}"
src="$work/src"
deps="$work/deps"
host=x86_64-w64-mingw32
jobs=$(nproc 2>/dev/null || echo 4)

# The win32 thread model of the mingw-w64 GCC: libstdc++ then needs no
# winpthread DLL.
cc=$host-gcc-win32
cxx=$host-g++-win32
command -v "$cc" >/dev/null || cc=$host-gcc
command -v "$cxx" >/dev/null || cxx=$host-g++

fetch() { # url sha256 file
  local url=$1 sha=$2 f=$3
  if [ -f "$f" ] && [ "$(sha256sum "$f" | cut -d' ' -f1)" = "$sha" ]; then return; fi
  curl -fsSL --retry 3 -o "$f.part" "$url"
  local got
  got=$(sha256sum "$f.part" | cut -d' ' -f1)
  if [ "$got" != "$sha" ]; then echo "SHA-256 mismatch for $url: $got, expected $sha" >&2; rm -f "$f.part"; exit 1; fi
  mv "$f.part" "$f"
}

unpack() { # name -> prints the source directory
  local i=$1 url sha root f
  url=$(m "w['deps'][$i]['url']"); sha=$(m "w['deps'][$i]['sha256']"); root=$(m "w['deps'][$i]['root']")
  f="$dl/$(m "w['deps'][$i]['name'].replace(' ', '_')")-$(basename "$url")"
  fetch "$url" "$sha" "$f"
  rm -rf "${src:?}/$root"
  tar -xf "$f" -C "$src"
  echo "$src/$root"
}

dep_index() { m "[d['name'] for d in w['deps']].index('$1')"; }

# The toolchain whose runtime parts are linked in must be the one declared in
# the manifest (and in packaging/licenses/binaries.json "bundled").
mapfile -t toolchain < <(m '"\n".join(k + " " + v for k, v in w["toolchain"].items())')
for line in "${toolchain[@]}"; do
  read -r pkg want <<<"$line"
  got=$(dpkg-query -W -f='${Version}' "$pkg" 2>/dev/null || echo missing)
  if [ "$got" != "$want" ]; then
    echo "toolchain package $pkg is $got, the manifest declares $want: update third-party.json (toolchain) and binaries.json (bundled) after checking its license" >&2
    exit 1
  fi
done

# Refuse GPL / non-free switches even if someone edits the manifest.
mapfile -t configure_args < <(m '"\n".join(w["configure"])')
for f in "${configure_args[@]}"; do
  case "$f" in --enable-gpl|--enable-nonfree|--enable-version3|--enable-libx264|--enable-libx265) echo "refusing $f (LGPL build only)" >&2; exit 1 ;; esac
done

rm -rf "${deps:?}" "${src:?}" "${out:?}"
mkdir -p "$dl" "$src" "$deps/include" "$deps/lib/pkgconfig" "$out/thirdparty"
export PKG_CONFIG_LIBDIR="$deps/lib/pkgconfig"
export PKG_CONFIG_PATH=""

echo "== zlib"
d=$(unpack "$(dep_index zlib)")
make -C "$d" -f win32/Makefile.gcc PREFIX="$host-" CC="$cc" libz.a -j"$jobs" >/dev/null
cp "$d/libz.a" "$deps/lib/"; cp "$d/zlib.h" "$d/zconf.h" "$deps/include/"
cp "$d/LICENSE" "$out/thirdparty/zlib-LICENSE.txt"

echo "== nv-codec-headers"
d=$(unpack "$(dep_index nv-codec-headers)")
make -C "$d" PREFIX="$deps" install >/dev/null
# The headers carry the MIT license text; ship it.
sed -n '1,/\*\//p' "$d/include/ffnvcodec/nvEncodeAPI.h" > "$out/thirdparty/nv-codec-headers-LICENSE.txt"

echo "== OpenH264"
d=$(unpack "$(dep_index OpenH264)")
make -C "$d" -j"$jobs" OS=mingw_nt ARCH=x86_64 CC="$cc" CXX="$cxx" AR="$host-ar" \
  PREFIX="$deps" BUILDTYPE=Release DEBUGSYMBOLS=False install-static >/dev/null
cp "$d/LICENSE" "$out/thirdparty/openh264-LICENSE.txt"

echo "== libvpl"
d=$(unpack "$(dep_index libvpl)")
cmake -S "$d" -B "$d/build" -DCMAKE_SYSTEM_NAME=Windows -DCMAKE_C_COMPILER="$cc" -DCMAKE_CXX_COMPILER="$cxx" \
  -DCMAKE_RC_COMPILER="$host-windres" -DCMAKE_BUILD_TYPE=Release -DCMAKE_INSTALL_PREFIX="$deps" \
  -DBUILD_SHARED_LIBS=OFF -DBUILD_TESTS=OFF -DBUILD_EXAMPLES=OFF -DINSTALL_EXAMPLES=OFF -DBUILD_TOOLS=OFF -DINSTALL_DEV=ON >/dev/null
cmake --build "$d/build" -j"$jobs" >/dev/null
cmake --install "$d/build" >/dev/null
cp "$d/LICENSE" "$out/thirdparty/libvpl-LICENSE.txt"

# C++ libraries: link libstdc++ statically (no libstdc++-6.dll next to ffmpeg).
for pc in "$deps"/lib/pkgconfig/*.pc; do
  sed -i -E 's/(^| )-lstdc\+\+( |$)/\1-l:libstdc++.a\2/g; s/(^| )-lpthread( |$)/\1\2/g' "$pc"
done
for pc in "$deps/lib/pkgconfig/vpl.pc" "$deps/lib/pkgconfig/openh264.pc"; do
  grep -q 'libstdc++' "$pc" && continue
  if grep -q '^Libs.private:' "$pc"; then sed -i -E 's/^Libs.private:(.*)$/Libs.private:\1 -l:libstdc++.a/' "$pc"
  else echo 'Libs.private: -l:libstdc++.a' >> "$pc"; fi
done

echo "== FFmpeg $version"
fetch "$(m 'w["url"]')" "$(m 'w["sha256"]')" "$dl/$(basename "$(m 'w["url"]')")"
root=$(m 'w["root"]')
tar -xf "$dl/$(basename "$(m 'w["url"]')")" -C "$src"
cd "$src/$root"
args=()
for a in "${configure_args[@]}"; do a=${a//PREFIX/$out}; args+=("${a//DEPS/$deps}"); done
./configure "${args[@]}" --cc="$cc" --cxx="$cxx" >"$work/configure.log" 2>&1 || { tail -40 ffbuild/config.log; exit 1; }
make -j"$jobs" >/dev/null
# LGPL only (SPEC-v3 V7): configure must have left GPL, version3 and non-free
# off, and the license string compiled into libavutil must be the LGPL one.
grep -q '^#define FFMPEG_LICENSE "LGPL version 2.1 or later"$' config.h || { grep FFMPEG_LICENSE config.h >&2; echo "not an LGPL 2.1 build" >&2; exit 1; }
for c in GPL NONFREE VERSION3; do
  grep -q "^!CONFIG_$c=yes$" ffbuild/config.mak || { echo "CONFIG_$c is enabled" >&2; exit 1; }
done
make install >/dev/null
cp COPYING.LGPLv2.1 LICENSE.md "$out/"
rm -rf "${out:?}/include" "${out:?}/lib" "${out:?}/share"
rm -f "${out:?}"/bin/*.lib

# Toolchain runtime licenses (static parts of libgcc, libstdc++, MinGW-w64).
for f in /usr/share/doc/mingw-w64-common/copyright /usr/share/doc/gcc-mingw-w64-base/copyright; do
  [ -f "$f" ] && cp "$f" "$out/thirdparty/$(basename "$(dirname "$f")")-copyright.txt"
done

# Every import must be a Windows system DLL or one of FFmpeg's own DLLs.
mapfile -t allowed < <(m '"\n".join(w["allowed_imports"])')
bad=0
for f in "$out"/bin/*.exe "$out"/bin/*.dll; do
  while read -r imp; do
    ok=0
    for pat in "${allowed[@]}"; do
      # shellcheck disable=SC2053
      if [[ "${imp,,}" == ${pat,,} ]]; then ok=1; break; fi
    done
    if [ $ok = 0 ]; then echo "unexpected import $imp in $(basename "$f")" >&2; bad=1; fi
  done < <($host-objdump -p "$f" | sed -n 's/^\s*DLL Name: //p')
done
[ $bad = 0 ] || exit 1

{
  echo "FFmpeg $version for Windows x64, built by desktop/scripts/build-ffmpeg-windows.sh"
  echo "compiler: $($cc --version | head -1)"
  dpkg-query -W -f='package: ${Package} ${Version}\n' gcc-mingw-w64-x86-64-win32 g++-mingw-w64-x86-64-win32 binutils-mingw-w64-x86-64 mingw-w64-x86-64-dev 2>/dev/null || true
  echo "license checks (SPEC-v3 V7): passed"
  echo "  configure line without --enable-gpl, --enable-nonfree, --enable-version3"
  echo "  config.h: FFMPEG_LICENSE \"LGPL version 2.1 or later\"; config.mak: CONFIG_GPL, CONFIG_NONFREE, CONFIG_VERSION3 off"
  echo "  imports of every .exe and .dll: Windows system DLLs and FFmpeg's own DLLs only"
  echo "configure:"; printf '  %s\n' "${args[@]}"
} > "$out/BUILDINFO.txt"
echo "built: $out"
ls -l "$out/bin"
