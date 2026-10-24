#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
#
# Builds the LGPL ffmpeg/ffprobe for macOS arm64 from the pinned source
# (packaging/third-party.json: url, sha256, configure). Run on macOS (CI:
# macos-14). Output: desktop/.cache/ffmpeg-macos-<version>/bin/{ffmpeg,ffprobe}.
# Not verified on a real Mac yet (SPEC-v3 appendix F): CI is the reference.
set -euo pipefail

cd "$(dirname "$0")/.."
manifest=packaging/third-party.json
read_json() { node -e "const m=require('./$manifest'); console.log($1)"; }

version=$(read_json "m.ffmpeg.version")
url=$(read_json "m.ffmpeg['darwin-arm64'].url")
sha=$(read_json "m.ffmpeg['darwin-arm64'].sha256")
root=$(read_json "m.ffmpeg['darwin-arm64'].root")
prefix="$PWD/.cache/ffmpeg-macos-$version"
work="$PWD/.cache/ffmpeg-src"
configure_args=()
while IFS= read -r a; do configure_args+=("${a/PREFIX/$prefix}"); done < <(read_json "m.ffmpeg['darwin-arm64'].configure.join('\n')")

for f in "${configure_args[@]}"; do
  case "$f" in --enable-gpl|--enable-nonfree|--enable-libx264|--enable-libx265) echo "refusing $f (LGPL build only)" >&2; exit 1 ;; esac
done

mkdir -p "$work"
tarball="$work/$(basename "$url")"
if [ ! -f "$tarball" ] || [ "$(shasum -a 256 "$tarball" | cut -d' ' -f1)" != "$sha" ]; then
  curl -fsSL -o "$tarball.part" "$url"
  got=$(shasum -a 256 "$tarball.part" | cut -d' ' -f1)
  if [ "$got" != "$sha" ]; then echo "SHA-256 mismatch for $url: $got" >&2; rm -f "$tarball.part"; exit 1; fi
  mv "$tarball.part" "$tarball"
fi
rm -rf "${work:?}/$root"
tar -xf "$tarball" -C "$work"
cd "$work/$root"
./configure "${configure_args[@]}"
make -j"$(sysctl -n hw.ncpu)"
make install
mkdir -p "$prefix/licenses"
cp COPYING.LGPLv2.1 LICENSE.md "$prefix/" 2>/dev/null || true
"$prefix/bin/ffmpeg" -hide_banner -buildconf
