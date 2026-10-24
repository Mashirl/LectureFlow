// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Stages everything that goes next to the app (electron-builder
// extraResources) into desktop/build/resources:
//
//   bin/lectureflow(.exe)   go build -trimpath -ldflags "-s -w -X main.version=V"
//   bin/ffmpeg, ffprobe     LGPL build from pinned sources (packaging/third-party.json;
//                           Windows: scripts/build-ffmpeg-windows.sh, macOS:
//                           scripts/build-ffmpeg-macos.sh), configure line and ffmpeg -L checked
//   bin/uv                  pinned release
//   worker/*.py             the runtime scripts, no tests
//   components.json, locks/ packaging/components/components.json and its lock files
//   sample/                 assets/sample (sample lecture, reference text, licence)
//   LICENSE, LICENSE-EXCEPTION.md, THIRD_PARTY_NOTICES.md, licenses/{ffmpeg,uv}/
//
// Usage: node scripts/stage.js [--platform win32|darwin]
'use strict';

const { execFileSync, spawnSync } = require('node:child_process');
const fs = require('node:fs');
const path = require('node:path');
const { fetchPinned, extract, globFiles, sha256File } = require('./lib/fetch');

const DESKTOP = path.resolve(__dirname, '..');
const REPO = path.resolve(DESKTOP, '..');
const CACHE = path.join(DESKTOP, '.cache');
const OUT = path.join(DESKTOP, 'build', 'resources');
const MANIFEST = JSON.parse(fs.readFileSync(path.join(DESKTOP, 'packaging', 'third-party.json'), 'utf8'));

// Python scripts the service runs (no tests, no conftest).
const WORKER_FILES = ['asr.py', 'asr_common.py', 'asr_openvino.py', 'asr_mlx.py', 'extract_pdf.py'];
const WORKER_OPTIONAL = ['colab_remote.py'];
const SAMPLE_FILES = ['sample-lecture.flac', 'reference.txt', 'LICENSE', 'SOURCE.md'];

function targetKey(platform) {
  if (platform === 'win32') return 'windows-x64';
  if (platform === 'darwin') return 'darwin-arm64';
  throw new Error(`unsupported target platform ${platform} (SPEC-v3 V0: Windows x64 and macOS arm64)`);
}

function copy(src, dest) {
  fs.mkdirSync(path.dirname(dest), { recursive: true });
  fs.copyFileSync(src, dest);
}

/**
 * gplFree(expr) → true when an SPDX expression can be met without GPL terms
 * (SPEC-v3 V7: the distributed ffmpeg contains no GPL component). LGPL is
 * fine; "X OR GPL-…" is fine through X; the GCC Runtime Library Exception
 * exempts the compiled program from the GPL.
 */
function gplFree(expr) {
  return String(expr).split(/\s+OR\s+/).some((alt) => alt.replace(/[()]/g, '').split(/\s+AND\s+/)
    .every((term) => !/^A?GPL-/.test(term.trim()) || /\sWITH\s+GCC-exception-3\.1$/.test(term.trim())));
}

/** gplComponents(binaries) → "binary / library [license]" of every ffmpeg part that needs the GPL. */
function gplComponents(binaries) {
  const out = [];
  for (const b of binaries.filter((x) => /^ffmpeg/i.test(x.name))) {
    if (!gplFree(b.license)) out.push(`${b.name} [${b.license}]`);
    for (const l of b.bundled || []) if (!gplFree(l.license)) out.push(`${b.name} / ${l.name} [${l.license}]`);
  }
  return out;
}

/** checkLicenseText(text) → true when ffmpeg -L names the GNU LGPL and no GPL-only terms. */
function checkLicenseText(text) {
  const t = String(text).replace(/\s+/g, ' ');
  return /GNU Lesser General Public License/.test(t) && !/version 3 of the License/.test(t) && !/GNU General Public License as published/.test(t);
}

/**
 * buildWindowsFfmpeg(spec): runs scripts/build-ffmpeg-windows.sh, directly
 * on Linux, through Docker (spec.docker_image, pinned by digest) elsewhere.
 */
function buildWindowsFfmpeg(spec) {
  const script = path.join(__dirname, 'build-ffmpeg-windows.sh');
  if (process.platform === 'linux') {
    execFileSync('bash', [script], { cwd: DESKTOP, stdio: 'inherit' });
    return;
  }
  const docker = spawnSync('docker', ['version', '--format', '{{.Server.Os}}'], { encoding: 'utf8' });
  if (docker.status !== 0 || docker.stdout.trim() !== 'linux') {
    throw new Error('the Windows ffmpeg is built from source on Linux (scripts/build-ffmpeg-windows.sh); start Docker with Linux containers, or put a finished build in desktop/.cache/ffmpeg-windows-<version>');
  }
  execFileSync('docker', ['run', '--rm', '-v', `${DESKTOP}:/src`, '-w', '/src', '-e', 'FFWIN_WORK=/tmp/ffwin', spec.docker_image,
    'bash', 'scripts/build-ffmpeg-windows.sh', '--install-deps'], { stdio: 'inherit', env: { ...process.env, MSYS_NO_PATHCONV: '1' } });
}

/** checkConfigure(line, forbidden) → the forbidden flags present in an ffmpeg configure line. */
function checkConfigure(line, forbidden) {
  const flags = String(line).split(/\s+/);
  return forbidden.filter((f) => flags.includes(f));
}

function goBuild(platform, version) {
  const exe = platform === 'win32' ? 'lectureflow.exe' : 'lectureflow';
  const out = path.join(OUT, 'bin', exe);
  const env = { ...process.env, CGO_ENABLED: '0', GOOS: platform === 'win32' ? 'windows' : 'darwin', GOARCH: platform === 'win32' ? 'amd64' : 'arm64' };
  console.log(`go build ${env.GOOS}/${env.GOARCH} version ${version}`);
  execFileSync('go', ['build', '-trimpath', '-ldflags', `-s -w -X main.version=${version}`, '-o', out, './cmd/lectureflow'], { cwd: REPO, env, stdio: 'inherit' });
}

async function stageFfmpeg(platform, key) {
  const spec = MANIFEST.ffmpeg[key];
  let root;
  if (platform === 'win32') {
    // Built from the pinned sources by scripts/build-ffmpeg-windows.sh
    // (Linux + mingw-w64; on Windows through Docker, CI: an ubuntu job).
    root = path.join(CACHE, `ffmpeg-windows-${MANIFEST.ffmpeg.version}`);
    if (!fs.existsSync(path.join(root, 'bin', 'ffmpeg.exe'))) buildWindowsFfmpeg(spec);
  } else {
    // Built from the pinned source by scripts/build-ffmpeg-macos.sh (CI).
    root = path.join(CACHE, `ffmpeg-macos-${MANIFEST.ffmpeg.version}`);
    if (!fs.existsSync(path.join(root, 'bin', 'ffmpeg'))) {
      if (process.platform !== 'darwin') throw new Error('the macOS ffmpeg is built on macOS (scripts/build-ffmpeg-macos.sh)');
      execFileSync('bash', [path.join(__dirname, 'build-ffmpeg-macos.sh')], { cwd: DESKTOP, stdio: 'inherit' });
    }
  }
  let n = 0;
  for (const pattern of spec.bin) {
    for (const f of globFiles(root, pattern)) {
      copy(f, path.join(OUT, 'bin', path.basename(f)));
      n++;
    }
  }
  if (n < 2) throw new Error(`ffmpeg: expected ffmpeg and ffprobe in ${root}`);
  for (const lic of spec.licenses) {
    for (const f of globFiles(root, lic)) copy(f, path.join(OUT, 'licenses', 'ffmpeg', path.basename(f)));
  }
  if (fs.existsSync(path.join(root, 'BUILDINFO.txt'))) copy(path.join(root, 'BUILDINFO.txt'), path.join(OUT, 'licenses', 'ffmpeg', 'BUILDINFO.txt'));
  // The configure line must not enable GPL or non-free parts (SPEC-v3 V7).
  const ffmpeg = path.join(OUT, 'bin', platform === 'win32' ? 'ffmpeg.exe' : 'ffmpeg');
  let configure = spec.configure ? spec.configure.join(' ') : '';
  if (process.platform === platform) {
    const r = spawnSync(ffmpeg, ['-hide_banner', '-buildconf'], { encoding: 'utf8' });
    if (r.status !== 0) throw new Error(`ffmpeg -buildconf failed: ${r.stderr}`);
    configure = r.stdout.split(/\r?\n/).map((l) => l.trim()).filter((l) => l.startsWith('--')).join(' ');
  }
  const bad = checkConfigure(configure, MANIFEST.ffmpeg.forbidden_configure);
  if (bad.length) throw new Error(`ffmpeg build is not LGPL-only: ${bad.join(' ')}`);
  const gpl = gplComponents(JSON.parse(fs.readFileSync(path.join(REPO, 'packaging', 'licenses', 'binaries.json'), 'utf8')).binaries);
  if (gpl.length) throw new Error(`ffmpeg declares GPL components (packaging/licenses/binaries.json): ${gpl.join('; ')}`);
  const checks = ['configure line without --enable-gpl / --enable-nonfree / --enable-version3', 'no GPL library in binaries.json bundled'];
  if (process.platform === platform) {
    // The license the binary reports (ffmpeg -L) must be the LGPL (V7).
    const r = spawnSync(ffmpeg, ['-hide_banner', '-L'], { encoding: 'utf8' });
    if (r.status !== 0 || !checkLicenseText(r.stdout)) throw new Error(`ffmpeg -L does not report the LGPL: ${r.stdout}`);
    checks.push('ffmpeg -L: GNU LGPL version 2.1 or later');
  }
  console.log(`ffmpeg license checks passed: ${checks.join('; ')}`);
  fs.mkdirSync(path.join(OUT, 'licenses', 'ffmpeg'), { recursive: true });
  fs.writeFileSync(path.join(OUT, 'licenses', 'ffmpeg', 'CONFIGURE.txt'),
    `FFmpeg ${MANIFEST.ffmpeg.version} (${spec.build || spec.url}), LGPL build. Configure flags:\n${configure.split(' ').join('\n')}\n`);
  return { configure, licenseChecks: checks };
}

async function stageUv(platform, key) {
  const spec = MANIFEST.uv[key];
  const archive = await fetchPinned(spec, path.join(CACHE, 'downloads'));
  const dir = path.join(CACHE, 'extract', `uv-${spec.sha256.slice(0, 12)}`);
  if (!fs.existsSync(dir)) extract(archive, dir);
  for (const b of spec.bin) {
    const f = path.join(dir, spec.root || '', b);
    if (!fs.existsSync(f)) throw new Error(`uv: ${b} missing in the archive`);
    copy(f, path.join(OUT, 'bin', path.basename(f)));
    if (platform !== 'win32') fs.chmodSync(path.join(OUT, 'bin', path.basename(f)), 0o755);
  }
  for (const lic of MANIFEST.uv.license_texts) {
    const f = await fetchPinned(lic, path.join(CACHE, 'downloads'));
    copy(f, path.join(OUT, 'licenses', 'uv', lic.name));
  }
}

function stageFiles() {
  for (const f of WORKER_FILES) copy(path.join(REPO, 'worker', f), path.join(OUT, 'worker', f));
  for (const f of WORKER_OPTIONAL) {
    if (fs.existsSync(path.join(REPO, 'worker', f))) copy(path.join(REPO, 'worker', f), path.join(OUT, 'worker', f));
  }
  copy(path.join(REPO, 'packaging', 'components', 'components.json'), path.join(OUT, 'components.json'));
  // The components manager looks for the lock files next to components.json.
  const locks = path.join(REPO, 'packaging', 'components', 'locks');
  for (const f of fs.readdirSync(locks)) {
    if (fs.statSync(path.join(locks, f)).isFile()) copy(path.join(locks, f), path.join(OUT, 'locks', f));
  }
  for (const f of SAMPLE_FILES) copy(path.join(REPO, 'assets', 'sample', f), path.join(OUT, 'sample', f));
  copy(path.join(REPO, 'LICENSE'), path.join(OUT, 'LICENSE'));
  copy(path.join(REPO, 'THIRD_PARTY_NOTICES.md'), path.join(OUT, 'THIRD_PARTY_NOTICES.md'));
  copy(path.join(REPO, 'LICENSE-EXCEPTION.md'), path.join(OUT, 'LICENSE-EXCEPTION.md'));
}

async function stage({ platform = process.platform, version } = {}) {
  const key = targetKey(platform);
  version = version || JSON.parse(fs.readFileSync(path.join(DESKTOP, 'package.json'), 'utf8')).version;
  fs.rmSync(OUT, { recursive: true, force: true });
  fs.mkdirSync(OUT, { recursive: true });
  goBuild(platform, version);
  const ff = await stageFfmpeg(platform, key);
  await stageUv(platform, key);
  stageFiles();
  const info = {
    version,
    platform: key,
    ffmpeg: { version: MANIFEST.ffmpeg.version, url: MANIFEST.ffmpeg[key].url, sha256: MANIFEST.ffmpeg[key].sha256, configure: ff.configure, license_checks: ff.licenseChecks },
    uv: { version: MANIFEST.uv.version, url: MANIFEST.uv[key].url, sha256: MANIFEST.uv[key].sha256 },
    server_sha256: sha256File(path.join(OUT, 'bin', platform === 'win32' ? 'lectureflow.exe' : 'lectureflow')),
  };
  fs.writeFileSync(path.join(DESKTOP, 'build', 'stage-info.json'), `${JSON.stringify(info, null, 2)}\n`);
  console.log(`staged ${OUT}`);
  return info;
}

module.exports = { stage, checkConfigure, checkLicenseText, gplFree, gplComponents, targetKey, WORKER_FILES, OUT };

if (require.main === module) {
  const i = process.argv.indexOf('--platform');
  stage({ platform: i > 0 ? process.argv[i + 1] : process.platform }).catch((err) => {
    console.error(`stage failed: ${err.message}`);
    process.exit(1);
  });
}
