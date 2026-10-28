# aria2c (bundled download tool)

The browser extension's download queue (SPEC-ext, `internal/fetch`) runs
[aria2](https://aria2.github.io/) in RPC mode. aria2c ships with the
installer next to ffmpeg and uv: `resources/bin/aria2c.exe` on Windows,
`resources/bin/aria2c` on macOS (`config.Config.Aria2c`; the toml key
`aria2c` or `aria2c` on PATH outside the desktop application).

Pins and source are in `desktop/packaging/third-party.json` (`aria2c`),
licenses in `packaging/licenses/binaries.json`; `desktop/scripts/stage.js`
puts the binary and its license texts into `resources/`.

| Platform | Binary | TLS |
|---|---|---|
| windows-x64 | aria2's own build `aria2-1.37.0-win-64bit-build1.zip`, unchanged; the zip and `aria2c.exe` are pinned by SHA-256 (the zip's equals Scoop's manifest) | WinTLS (SChannel) |
| darwin-arm64 | built from `aria2-1.37.0.tar.xz` (SHA-256 equal to Homebrew's formula) by `build-aria2c-macos.sh`, for macOS 13.0 or later (the app's minimum) | AppleTLS (Security.framework) |

## License

aria2 is GPL-2.0-or-later with an additional permission to link with
OpenSSL (the SPDX field says `GPL-2.0-or-later`, the permission is in the
notes: neither build links OpenSSL). It is a separate program that
LectureFlow starts and talks to over JSON-RPC.

Corresponding source, as for ffmpeg: every release carries it as an
attachment next to the installers (GPLv2 section 3: the source offered
from the same place), `LectureFlow-<version>-source-aria2c-windows-x64.tar`
and `…-aria2c-darwin-arm64.tar`, built by `desktop/scripts/sources.js` in
`release.yml`: the pinned upstream tarballs, unchanged and checked by
SHA-256 (aria2, and on Windows the libraries upstream linked in, at the
versions of the build's `README.mingw`), with `third-party.json` (pins,
configure line) and, for macOS, `build-aria2c-macos.sh`; upstream's
`Dockerfile.mingw` / `mingw-config` (the Windows build scripts) are in the
aria2 tarball. The GCC and MinGW-w64 runtime sources are not attached (the
GCC Runtime Library Exception and the ZPL-2.1 ask for none); `SOURCE.txt`
says so and where they are. The installer carries
`resources/licenses/aria2/`: `COPYING`, `LICENSE.OpenSSL`, `AUTHORS`,
`SOURCE.txt` (the attachment of that version and the upstream addresses),
`BUILDINFO.txt` (macOS) and `thirdparty/` (the license texts of everything
linked in).

## The macOS build

`bash packaging/aria2c/build-aria2c-macos.sh` (needs Xcode's command line
tools and node) writes `desktop/.cache/aria2c-macos-<version>/`. It runs
configure and make with `PATH=/usr/bin:/bin:/usr/sbin:/sbin` and no flags
from the environment, so Homebrew libraries cannot leak in, and fails
unless:

- configure reports AppleTLS and none of OpenSSL, GnuTLS, nettle, GMP,
  libgcrypt, libxml2, Expat, SQLite, c-ares, libssh2, BitTorrent, Metalink;
- `otool -L` lists only `/usr/lib/` and `/System/Library/` (libz, libc++,
  libSystem, CoreFoundation, Security);
- the minimum macOS (`LC_BUILD_VERSION minos`) equals
  `macos.minimum_system_version` of `third-party.json` (the app's
  `minimumSystemVersion`, 13.0; the build uses it as
  `MACOSX_DEPLOYMENT_TARGET` and `-mmacosx-version-min`), and the binary is
  arm64;
- `aria2c --version` says 1.37.0 with HTTPS and AppleTLS.

The binary is stripped and signed ad hoc. CI builds it in `ci.yml` (job
`aria2c-macos`) and `release.yml` reads the same cache (key: the hash of
this script and of `third-party.json`).

**SHA-256.** The core component of `packaging/components/components.json`
records the SHA-256 and size of the official `aria2c.exe`
(`platforms.windows-x64.files`, name `bin/aria2c.exe`); `stage.js` fails
when the staged file differs, and the diagnostics compare the file in use
with it. There is no macOS entry: the build is not byte-for-byte
reproducible across Xcode versions, and electron-builder signs every
Mach-O in the app again while packaging (measured: the ad-hoc re-signing
turned b198bc40… into 769bea4c…, 1 546 192 into 1 547 376 bytes), so no
SHA-256 known before packaging matches the shipped file. On macOS the
diagnostics show the SHA-256 of the file itself; the code signature
covers its integrity. electron-builder also Authenticode-signs every
`.exe` in `resources/` when a Windows certificate is configured, which
would change `aria2c.exe`; `win.signExts: ["!aria2c.exe"]` in
`desktop/electron-builder.config.js` keeps it byte-identical to aria2's
release (checked against electron-builder 26.17.0, see NOTES; not set
yet: builds are unsigned today).

Verified on 2026-10-26 on Apple M6, macOS 27.0.1, Apple clang 21.0.0: build
26 s, 1.5 MB, HTTPS downloads (single and 4 connections) correct,
certificates checked against the system trust store (expired, self-signed
and wrong-host test sites refused), JSON-RPC with `rpc-secret` from
`--conf-path` and `header` (Cookie, Referer) in `addUri` working.

## Updating to a newer aria2

1. Windows: download the new `aria2-X-win-64bit-buildN.zip`, compare its
   SHA-256 with an independent source (Scoop's `bucket/aria2.json`), unpack
   it into an empty directory and note the SHA-256 and size of
   `aria2c.exe`, the libraries in `README.mingw` and the toolchain in
   `aria2c --version`. macOS: the source tarball's SHA-256 (Homebrew's
   formula).
2. Edit `aria2c` in `desktop/packaging/third-party.json` (pins, `deps` with
   their source tarballs and license files, `license_texts`, `toolchain`,
   `source_code`), the two `aria2c` entries in
   `packaging/licenses/binaries.json`, and the core component's
   `bin/aria2c.exe` entry in `packaging/components/manifest.src.json` and
   `components.json`.
3. Build on a Mac (`build-aria2c-macos.sh`), then
   `powershell -File make.ps1 audit` and `cd desktop && npm test`
   (`build.test.js` checks that the files agree).
