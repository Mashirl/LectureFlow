// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The corresponding source of the GPL / LGPL programs in resources/bin
// (ffmpeg: LGPL-2.1-or-later, aria2c: GPL-2.0-or-later), as GitHub release
// attachments next to the installers (packaging/third-party.json
// release_sources): GPLv2 section 3 and LGPL-2.1 section 4 accept source
// offered "from the same place" as the binaries.
//
// One tar per program and platform, LectureFlow-<version>-source-<bundle>.tar:
//
//   <name>/README.txt     what the binary is, where each file comes from, how it is built
//   <name>/SHA256SUMS     of every file below
//   <name>/upstream/      the pinned upstream tarballs the build uses, unchanged
//                         (the program's own source and its deps, verified by SHA-256)
//   <name>/lectureflow/   the build script and third-party.json (the pins, configure lines)
//
// The tarballs are already compressed, so the outer tar is not. Entries are
// written in a fixed order with a fixed time (SOURCE_DATE_EPOCH), so the
// same inputs give the same bytes. Each tar is written as .partial and
// renamed when complete.
//
// Usage: node scripts/sources.js --out DIR [--version V] [--notes FILE]
'use strict';

const fs = require('node:fs');
const path = require('node:path');
const { fetchPinned, sha256File, cacheName } = require('./lib/fetch');

const DESKTOP = path.resolve(__dirname, '..');
const REPO = path.resolve(DESKTOP, '..');
const MANIFEST = JSON.parse(fs.readFileSync(path.join(DESKTOP, 'packaging', 'third-party.json'), 'utf8'));
const PROGRAMS = ['ffmpeg', 'aria2c'];
const KEYS = ['windows-x64', 'darwin-arm64'];
const PLATFORM_NAMES = { 'windows-x64': 'Windows x64', 'darwin-arm64': 'macOS arm64' };

/** assetName(version, bundle) → the release attachment's file name (release_sources.asset). */
function assetName(version, bundle, manifest = MANIFEST) {
  if (!/^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$/.test(String(version))) throw new Error(`bad version ${JSON.stringify(version)}`);
  return manifest.release_sources.asset.replace('{version}', version).replace('{bundle}', bundle);
}

/**
 * releaseRepo(env) → owner/name of the repository whose releases carry the
 * source: the GitHub Actions run's own repository (the release is created
 * there), otherwise release_sources.repository.
 */
function releaseRepo(env = process.env, manifest = MANIFEST) {
  const r = env.GITHUB_REPOSITORY;
  return r && /^[A-Za-z0-9-]+\/[A-Za-z0-9._-]+$/.test(r) ? r : manifest.release_sources.repository;
}

function releaseURL(version, repo) {
  return `https://github.com/${repo}/releases/tag/v${version}`;
}

function assetURL(version, bundle, repo, manifest = MANIFEST) {
  return `https://github.com/${repo}/releases/download/v${version}/${assetName(version, bundle, manifest)}`;
}

/**
 * archiveName(name, url) → the file name under upstream/: the URL's last
 * segment, prefixed with the library's name when it does not contain it
 * (GitHub tag archives are called v1.3.1.tar.gz).
 */
function archiveName(name, url) {
  const base = cacheName(url);
  const flat = (s) => s.toLowerCase().replace(/[^a-z0-9]+/g, '');
  return flat(base).includes(flat(name)) ? base : `${name.replace(/[^A-Za-z0-9._-]+/g, '-')}-${base}`;
}

/**
 * bundles(manifest) → one entry per program and platform: the pinned
 * tarballs the build uses (own source first, then deps; one file per
 * SHA-256: wslay is inside the aria2 tarball), the build scripts, and the
 * source_code entries that are not attached (with the reason).
 */
function bundles(manifest = MANIFEST) {
  const out = [];
  for (const program of PROGRAMS) {
    const top = manifest[program];
    for (const key of KEYS) {
      const spec = top[key];
      const own = program === 'aria2c'
        ? { name: 'aria2', version: top.version, url: top.source.url, sha256: top.source.sha256 }
        : { name: 'FFmpeg', version: top.version, url: spec.url, sha256: spec.sha256 };
      const files = [];
      for (const f of [own, ...(spec.deps || [])]) {
        if (files.some((x) => x.sha256 === f.sha256)) continue;
        files.push({ name: f.name, version: f.version, license: f.license || top.license, url: f.url, sha256: f.sha256, archive: archiveName(f.name, f.url) });
      }
      out.push({
        id: `${program}-${key}`,
        program,
        key,
        version: top.version,
        license: top.license,
        build: spec.build,
        files,
        scripts: spec.build_scripts || [],
        notAttached: (spec.source_code || []).filter((s) => s.not_attached),
        configure: spec.configure || null,
        toolchain: spec.toolchain || null,
      });
    }
  }
  return out;
}

/**
 * bundleProblems(manifest, repoRoot) → why the release attachments would
 * not be the corresponding source the manifests declare: every source_code
 * entry is the attachment itself (release_asset, exactly one), a pinned
 * tarball in the bundle, or says why it is not attached; every tarball in
 * the bundle is listed in source_code; build scripts exist.
 */
function bundleProblems(manifest = MANIFEST, repoRoot = REPO) {
  const problems = [];
  for (const b of bundles(manifest)) {
    const sc = manifest[b.program][b.key].source_code || [];
    const urls = new Set(b.files.map((f) => f.url));
    const assets = sc.filter((s) => s.release_asset);
    if (assets.length !== 1) problems.push(`${b.id}: source_code needs exactly one release_asset entry, has ${assets.length}`);
    for (const s of sc) {
      if (s.release_asset) {
        if (s.url !== `https://github.com/${manifest.release_sources.repository}/releases`) problems.push(`${b.id}: the release_asset entry must point to the releases of ${manifest.release_sources.repository}`);
        if (!s.what.includes(manifest.release_sources.asset.replace('{bundle}', b.id).replace('{version}', '<version>'))) problems.push(`${b.id}: the release_asset entry does not name its attachment`);
      } else if (!urls.has(s.url) && !s.not_attached) {
        problems.push(`${b.id}: source_code ${s.url} is neither attached (no pinned tarball) nor explained (not_attached)`);
      } else if (urls.has(s.url) && s.not_attached) {
        problems.push(`${b.id}: source_code ${s.url} is attached but says not_attached`);
      }
    }
    for (const f of b.files) {
      if (!sc.some((s) => s.url === f.url)) problems.push(`${b.id}: ${f.url} is attached but not listed in source_code`);
      if (!/^https:\/\//.test(f.url) || !/^[0-9a-f]{64}$/.test(f.sha256)) problems.push(`${b.id}: ${f.name} is not pinned by an HTTPS URL and a SHA-256`);
    }
    const names = b.files.map((f) => f.archive);
    if (new Set(names).size !== names.length) problems.push(`${b.id}: two tarballs share a file name: ${names.join(', ')}`);
    if (!b.scripts.includes('desktop/packaging/third-party.json')) problems.push(`${b.id}: build_scripts must include desktop/packaging/third-party.json (the pins)`);
    for (const s of b.scripts) {
      if (s.includes('..') || path.isAbsolute(s) || !fs.existsSync(path.join(repoRoot, ...s.split('/')))) problems.push(`${b.id}: build script ${s} not found`);
    }
  }
  return problems;
}

const HOW = {
  'ffmpeg-windows-x64': 'Run desktop/scripts/build-ffmpeg-windows.sh --install-deps from a LectureFlow checkout of this version in the Docker image third-party.json names (ffmpeg.windows-x64.docker_image; desktop/scripts/stage.js does this when Docker is available). The script fetches the tarballs above, checks their SHA-256 and builds with the configure line below.',
  'ffmpeg-darwin-arm64': 'Run bash desktop/scripts/build-ffmpeg-macos.sh from a LectureFlow checkout of this version on an Apple silicon Mac (Xcode command line tools, Node.js, Python 3.10 or newer for meson). The script fetches the tarballs above, checks their SHA-256 and builds with the configure line below.',
  'aria2c-windows-x64': 'aria2c.exe is aria2\'s own Windows build (aria2-1.37.0-win-64bit-build1), shipped unchanged. Upstream built it with Dockerfile.mingw and mingw-config from upstream/aria2-1.37.0.tar.xz (HOST=x86_64-w64-mingw32), which download and statically link the library versions in upstream/; README.mingw in the installer describes it.',
  'aria2c-darwin-arm64': 'Run bash packaging/aria2c/build-aria2c-macos.sh from a LectureFlow checkout of this version on an Apple silicon Mac (Xcode command line tools, Node.js). The script fetches the tarball above, checks its SHA-256 and builds with the configure line below (BUILDINFO.txt in the installer records the compiler and SDK).',
};

/** readmeText(bundle, version, repo) → README.txt of a source attachment. */
function readmeText(b, version, repo, manifest = MANIFEST) {
  const bins = b.program === 'ffmpeg'
    ? (b.key === 'windows-x64' ? 'resources/bin/ffmpeg.exe, ffprobe.exe and the FFmpeg DLLs' : 'resources/bin/ffmpeg and ffprobe')
    : (b.key === 'windows-x64' ? 'resources/bin/aria2c.exe' : 'resources/bin/aria2c');
  const lines = [
    `LectureFlow ${version}: corresponding source of ${b.program === 'ffmpeg' ? 'FFmpeg' : 'aria2'} ${b.version} (${PLATFORM_NAMES[b.key]})`,
    '',
    `The LectureFlow ${version} installers for ${PLATFORM_NAMES[b.key]} ship ${bins} (${b.license}).`,
    `This file is attached to the release next to them: ${releaseURL(version, repo)}`,
    `(${assetName(version, b.id, manifest)}). It holds the source code of that build:`,
    '',
    '  upstream/      the upstream source tarballs, unchanged (SHA-256 below and in SHA256SUMS)',
    '  lectureflow/   the LectureFlow files that pin and build them',
    '',
    'Upstream source tarballs:',
  ];
  for (const f of b.files) lines.push('', `  upstream/${f.archive}`, `    ${f.name} ${f.version}, ${f.license}`, `    from ${f.url}`, `    SHA-256 ${f.sha256}`);
  lines.push('', 'LectureFlow files:', ...b.scripts.map((s) => `  lectureflow/${s}`), '', `Build: ${b.build}.`, '', HOW[b.id]);
  if (b.configure) lines.push('', 'Configure flags:', ...b.configure.map((f) => `  ${f}`));
  if (b.toolchain) lines.push('', 'Toolchain:', ...Object.entries(b.toolchain).map(([k, v]) => `  ${k} ${v}`));
  if (b.notAttached.length) {
    lines.push('', 'Not attached:');
    for (const s of b.notAttached) lines.push(`- ${s.what}: ${s.url}`, `  ${s.not_attached}`);
  }
  lines.push('', `The license texts of ${b.program === 'ffmpeg' ? 'FFmpeg' : 'aria2'} and of the libraries in it are in the installer, under resources/licenses/${b.program === 'ffmpeg' ? 'ffmpeg' : 'aria2'}/ (SOURCE.txt there points here).`, '');
  return lines.join('\n');
}

// --- tar (POSIX ustar) ---------------------------------------------------------

function octal(buf, off, len, value) {
  const s = Math.floor(value).toString(8);
  if (s.length > len - 1) throw new Error(`tar: ${value} does not fit in ${len - 1} octal digits`);
  buf.write(`${s.padStart(len - 1, '0')}\0`, off, len, 'ascii');
}

/** tarHeader({name, size, mode, mtime}) → the 512-byte ustar header of a regular file. */
function tarHeader({ name, size, mode, mtime }) {
  if (!/^[\x21-\x7e][\x20-\x7e]*$/.test(name) || name.startsWith('/') || name.split('/').includes('..')) throw new Error(`tar: bad entry name ${JSON.stringify(name)}`);
  let prefix = '';
  let base = name;
  if (base.length > 100) {
    // Split at a slash: prefix (at most 155 bytes) / name (at most 100).
    const i = name.lastIndexOf('/', 155);
    if (i < 0 || name.length - i - 1 > 100) throw new Error(`tar: entry name too long: ${name}`);
    prefix = name.slice(0, i);
    base = name.slice(i + 1);
  }
  const h = Buffer.alloc(512, 0);
  h.write(base, 0, 100, 'ascii');
  octal(h, 100, 8, mode);
  octal(h, 108, 8, 0); // uid
  octal(h, 116, 8, 0); // gid
  octal(h, 124, 12, size);
  octal(h, 136, 12, mtime);
  h.fill(0x20, 148, 156); // checksum field counts as spaces
  h.write('0', 156, 1, 'ascii'); // regular file
  h.write('ustar\x0000', 257, 8, 'ascii');
  h.write('root', 265, 32, 'ascii');
  h.write('root', 297, 32, 'ascii');
  h.write(prefix, 345, 155, 'ascii');
  let sum = 0;
  for (const byte of h) sum += byte;
  h.write(`${sum.toString(8).padStart(6, '0')}\0 `, 148, 8, 'ascii');
  return h;
}

/**
 * writeTar(file, entries, mtime): entries [{name, file | data, mode}] into
 * a ustar archive; written to file.partial and renamed when complete.
 */
function writeTar(file, entries, mtime = 0) {
  const part = `${file}.partial`;
  fs.rmSync(part, { force: true });
  const fd = fs.openSync(part, 'w');
  try {
    const buf = Buffer.alloc(1 << 20);
    for (const e of entries) {
      const size = e.data ? e.data.length : fs.statSync(e.file).size;
      fs.writeSync(fd, tarHeader({ name: e.name, size, mode: e.mode || 0o644, mtime }));
      if (e.data) {
        fs.writeSync(fd, e.data);
      } else {
        const src = fs.openSync(e.file, 'r');
        try {
          let left = size;
          while (left > 0) {
            const n = fs.readSync(src, buf, 0, Math.min(buf.length, left), null);
            if (n <= 0) throw new Error(`tar: ${e.file} shrank while being read`);
            fs.writeSync(fd, buf.subarray(0, n));
            left -= n;
          }
        } finally {
          fs.closeSync(src);
        }
      }
      if (size % 512) fs.writeSync(fd, Buffer.alloc(512 - (size % 512)));
    }
    fs.writeSync(fd, Buffer.alloc(1024)); // end of archive
  } catch (err) {
    fs.closeSync(fd);
    fs.rmSync(part, { force: true });
    throw err;
  }
  fs.closeSync(fd);
  fs.renameSync(part, file);
}

/**
 * buildSources({outDir, version}) → [{id, asset, bytes, files}]: writes the
 * source attachments of every bundle into outDir. Downloads are pinned
 * (fetchPinned: HTTPS, SHA-256 checked before use) and cached in cacheDir.
 */
async function buildSources({
  outDir, version, repo = releaseRepo(), cacheDir = path.join(DESKTOP, '.cache', 'downloads'), mtime = 0,
  manifest = MANIFEST, repoRoot = REPO, fetchImpl, log = console.log,
}) {
  const problems = bundleProblems(manifest, repoRoot);
  if (problems.length) throw new Error(`third-party.json: ${problems.join('; ')}`);
  fs.mkdirSync(outDir, { recursive: true });
  const results = [];
  for (const b of bundles(manifest)) {
    const asset = assetName(version, b.id, manifest);
    const root = asset.replace(/\.tar$/, '');
    const body = [];
    const sums = [];
    for (const f of b.files) {
      const file = await fetchPinned(f, cacheDir, fetchImpl ? { fetchImpl, log } : { log });
      body.push({ name: `${root}/upstream/${f.archive}`, file, mode: 0o644 });
      sums.push(`${f.sha256}  upstream/${f.archive}`);
    }
    for (const s of b.scripts) {
      const file = path.join(repoRoot, ...s.split('/'));
      body.push({ name: `${root}/lectureflow/${s}`, file, mode: s.endsWith('.sh') ? 0o755 : 0o644 });
      sums.push(`${sha256File(file)}  lectureflow/${s}`);
    }
    const out = path.join(outDir, asset);
    writeTar(out, [
      { name: `${root}/README.txt`, data: Buffer.from(readmeText(b, version, repo, manifest)) },
      { name: `${root}/SHA256SUMS`, data: Buffer.from(`${sums.join('\n')}\n`) },
      ...body,
    ], mtime);
    results.push({ id: b.id, asset, bytes: fs.statSync(out).size, files: b.files.map((f) => f.archive), scripts: b.scripts });
    log(`${asset}: ${(fs.statSync(out).size / 1048576).toFixed(1)} MB`);
  }
  return results;
}

/** renderNotes(results, version) → the release notes section about the source attachments. */
function renderNotes(results) {
  const what = {
    'ffmpeg-windows-x64': 'ffmpeg / ffprobe in the Windows installers (LGPL-2.1-or-later)',
    'ffmpeg-darwin-arm64': 'ffmpeg / ffprobe in the macOS dmg and zip (LGPL-2.1-or-later)',
    'aria2c-windows-x64': 'aria2c.exe in the Windows installers (GPL-2.0-or-later)',
    'aria2c-darwin-arm64': 'aria2c in the macOS dmg and zip (GPL-2.0-or-later)',
  };
  const total = results.reduce((n, r) => n + r.bytes, 0);
  return [
    '## Source code of the bundled ffmpeg and aria2c',
    '',
    'The corresponding source code of the third-party programs under GPL / LGPL in the installers: the upstream tarballs each build uses (unchanged, with their SHA-256), the build script and its pins. README.txt in each archive explains it.',
    '',
    '| Attachment | Source of | Size | Upstream tarballs |',
    '|---|---|---|---|',
    ...results.map((r) => `| ${r.asset} | ${what[r.id] || r.id} | ${(r.bytes / 1048576).toFixed(1)} MB | ${r.files.join(', ')} |`),
    '',
    `Together ${(total / 1048576).toFixed(1)} MB.`,
    '',
  ].join('\n');
}

module.exports = {
  assetName, assetURL, releaseRepo, releaseURL, archiveName, bundles, bundleProblems, readmeText,
  tarHeader, writeTar, buildSources, renderNotes,
};

if (require.main === module) {
  const arg = (name) => {
    const i = process.argv.indexOf(name);
    return i > 0 ? process.argv[i + 1] : undefined;
  };
  const outDir = arg('--out');
  if (!outDir) {
    console.error('usage: node scripts/sources.js --out DIR [--version V] [--notes FILE]');
    process.exit(2);
  }
  const version = arg('--version') || JSON.parse(fs.readFileSync(path.join(DESKTOP, 'package.json'), 'utf8')).version;
  const epoch = Number.parseInt(process.env.SOURCE_DATE_EPOCH || '0', 10);
  buildSources({ outDir: path.resolve(outDir), version, mtime: Number.isFinite(epoch) && epoch > 0 ? epoch : 0 }).then((results) => {
    const notes = arg('--notes');
    if (notes) fs.writeFileSync(notes, renderNotes(results));
  }).catch((err) => {
    console.error(`sources failed: ${err.message}`);
    process.exit(1);
  });
}
