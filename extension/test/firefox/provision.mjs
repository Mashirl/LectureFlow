// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The Firefox that `npm run test:firefox` runs: a copy of its own, never the
// Firefox installed on the machine. A run of the installed Firefox, even
// with a temporary profile, shares that installation with the user's own
// browser: its update directory (an update the user's Firefox downloaded
// can be staged or applied by the test's instance, after which the user's
// running Firefox can no longer open tabs until it restarts) and its
// per-installation scheduled tasks. A copy in its own directory has none of
// that, and this module makes sure the copy never updates itself.
//
//   node test/firefox/provision.mjs     (npm run firefox:prepare)
//
// downloads the copy if it is not there yet and prints where it is; the
// test calls firefoxForTests() itself, so this is only needed to prepare
// ahead of time (or to check an offline download, see preparationHelp).
//
// The copy: Firefox ESR, pinned below, from archive.mozilla.org, checked
// against SHA-512 values taken from Mozilla's SHA512SUMS (whose signature by
// Mozilla's release key, 14F2 6682 D091 6CDD 81E3 7B6D 61B7 B526 D98F 0353,
// was checked when the pin was set). It lives in <checkout>/tools/firefox/
// (tools/ is ignored by git, as for the portable Node and Playwright's
// browsers; a git worktree uses its main checkout's), or LF_FIREFOX_CACHE.
// Unpacked without running anything downloaded where possible: Windows'
// own tar.exe (libarchive) reads the installer's 7-Zip archive; the
// installer's documented extract-only mode (/ExtractDir=, installs nothing)
// is the fallback for a tar.exe without LZMA. macOS: hdiutil attach of the
// disk image, read-only, then ditto. Linux: tar -xJf.
// Then, in the copy only: distribution/policies.json with DisableAppUpdate
// (and DisableDefaultBrowserAgent); on Windows and Linux the updater and the
// maintenance service binaries are removed, so the copy cannot apply an
// update even where policies.json is ignored (on Windows, any Firefox
// policy in the registry makes Firefox ignore the file). The profile prefs
// the test adds are UPDATE_PREFS.
//
// LF_FIREFOX=<path or command> uses another Firefox instead; nothing of
// this module touches it. Node standard library only.

import fs from 'node:fs';
import path from 'node:path';
import crypto from 'node:crypto';
import { spawnSync } from 'node:child_process';
import { Readable } from 'node:stream';
import { fileURLToPath } from 'node:url';

// Firefox ESR, not the release channel: the version the extension has been
// checked against since M4 (153 ESR), the channel managed machines (at
// universities, say) tend to run, and it changes only by security fixes for
// a year, so a pinned copy stays representative between pin updates. The
// release channel (159 when this was pinned) is what most AMO users have;
// moving the pin there is a one-line change if that matters more. To move the
// pin: take the new version's SHA512SUMS (and SHA512SUMS.asc, check it with
// the key above) from archive.mozilla.org/pub/firefox/releases/<version>/,
// copy the five lines below, and run npm run test:firefox.
const VERSION = '153.6.0esr';
export const FIREFOX = Object.freeze({
  version: VERSION,
  base: `https://archive.mozilla.org/pub/firefox/releases/${VERSION}/`,
  packages: Object.freeze({
    win64: { file: `win64/en-US/Firefox Setup ${VERSION}.exe`, sha512: 'd6d73183ae947775bb93f95879a88d6d60b1f64276c36997bfd472097d1945a94d902b95c2174fd63181e6f6bcbb6695843cd3c711118201f23dcbba46fae1e4' },
    'win64-aarch64': { file: `win64-aarch64/en-US/Firefox Setup ${VERSION}.exe`, sha512: 'cfda9c7a79bfa3d5ee17dd6009c2782fd2d5c7cff2697b00a2cee8a52a9d3c0b589ef93ab47a8cf70fa25f7a3b40490ffaa334c6c7303012bfab8d12d17ce267' },
    mac: { file: `mac/en-US/Firefox ${VERSION}.dmg`, sha512: '9e983be7df1d98d69e91169b2bd102f9a07fef10b605164b59c5ffb309aedc25cc3155bf23e0afdfcb11804733df5dd77c249f190bb9b2c13ce3bd2be096c5b6' },
    'linux-x86_64': { file: `linux-x86_64/en-US/firefox-${VERSION}.tar.xz`, sha512: 'b2ed0f8d5dd1f363aae0e5d09554eee00c6beb980d5ea40db2a5dd7cc478b01b4f7059f0c981d97aec8af2ef64629de4349aedf075be8ebdd39cc3d814a6a17f' },
    'linux-aarch64': { file: `linux-aarch64/en-US/firefox-${VERSION}.tar.xz`, sha512: 'e605dabf158a03ea9330e0868cd9fc3d5b2c9fd6d36d5cbbbce4124f354b515e0dd74f4b5331be8c391da904352060d301957c905a0f32f2fd9f9885e8d07459' },
  }),
});

/** Written into the copy's distribution directory. */
export const POLICIES = Object.freeze({
  policies: {
    // Services.policies.isAllowed('appUpdate') = false from the first moment:
    // the update service and its startup stub stay off (nothing pending is
    // resumed, nothing is checked or downloaded).
    DisableAppUpdate: true,
    // The agent's scheduled task is registered only by the installer, which
    // the copy never runs; this keeps it so should that change.
    DisableDefaultBrowserAgent: true,
  },
});

/**
 * Prefs for the test profile, on top of the policy (checked against the
 * 153 ESR sources in omni.ja; see NOTES "ext firefox-isolated"):
 * - app.update.disabledForTesting: the update service is off while the
 *   Remote Agent runs (the test starts it), and the startup task that
 *   schedules background updates is skipped. The Remote Agent forces this
 *   one too, but only once it has started; user.js has it from the start.
 * - app.update.auto: a profile pref on macOS and Linux. On Windows the
 *   setting is per installation (update-config.json under C:\ProgramData\
 *   Mozilla-…\updates\<hash of the installation path>), and the profile
 *   pref only seeds that file when it is first created: the copy's first
 *   run writes it with false; an existing installation keeps its own.
 * - staging, maintenance service, BITS: what the update service would use
 *   to stage or download an update if one were ever found.
 * Not set: app.update.enabled (removed in Firefox 63; web-ext still sets
 * it, without effect) and app.update.background.scheduling.enabled (no
 * longer read; Firefox only clears it).
 */
export const UPDATE_PREFS = Object.freeze({
  'app.update.disabledForTesting': true,
  'app.update.auto': false,
  'app.update.staging.enabled': false,
  'app.update.service.enabled': false,
  'app.update.BITS.enabled': false,
});

const MARKER = 'lectureflow-firefox.json';

/** The package for this machine: { key, file, sha512, url, name }, or null. */
export function packageFor(platform = process.platform, arch = process.arch) {
  const key = {
    'win32/x64': 'win64',
    'win32/arm64': 'win64-aarch64',
    'darwin/x64': 'mac',
    'darwin/arm64': 'mac',
    'linux/x64': 'linux-x86_64',
    'linux/arm64': 'linux-aarch64',
  }[`${platform}/${arch}`];
  if (!key) return null;
  const p = FIREFOX.packages[key];
  return { key, file: p.file, sha512: p.sha512, name: p.file.split('/').pop(), url: FIREFOX.base + p.file.split('/').map(encodeURIComponent).join('/') };
}

/** The Firefox executable inside an unpacked copy (appDir). */
export function appBinary(appDir, platform = process.platform) {
  if (platform === 'win32') return path.join(appDir, 'firefox.exe');
  if (platform === 'darwin') return path.join(appDir, 'Firefox.app', 'Contents', 'MacOS', 'firefox');
  return path.join(appDir, 'firefox');
}

/** Where Firefox looks for policies.json in that copy. */
export function distributionDir(appDir, platform = process.platform) {
  if (platform === 'darwin') return path.join(appDir, 'Firefox.app', 'Contents', 'Resources', 'distribution');
  return path.join(appDir, 'distribution');
}

/**
 * Files removed from the copy so that it cannot apply an update. Not on
 * macOS: they sit inside the signed bundle (Contents/MacOS/updater.app),
 * and the policy is enough there (no registry policies to override it).
 */
export function updaterFiles(appDir, platform = process.platform) {
  if (platform === 'win32') return ['updater.exe', 'updater.ini', 'maintenanceservice.exe', 'maintenanceservice_installer.exe'].map((f) => path.join(appDir, f));
  if (platform === 'darwin') return [];
  return ['updater', 'updater.ini'].map((f) => path.join(appDir, f));
}

/**
 * The main checkout for `top` (the repository root holding extension/): a
 * git worktree (.git is a file "gitdir: <main>/.git/worktrees/<name>") uses
 * its main checkout's tools/, so worktrees share one copy.
 */
export function mainCheckout(top) {
  try {
    const dotgit = path.join(top, '.git');
    if (!fs.statSync(dotgit).isFile()) return top;
    const m = /^gitdir:\s*(.+?)\s*$/m.exec(fs.readFileSync(dotgit, 'utf8'));
    if (!m) return top;
    const gitdir = path.resolve(top, m[1]);
    const parts = gitdir.split(/[\\/]+/);
    const i = parts.lastIndexOf('worktrees');
    if (i < 1 || parts[i - 1] !== '.git') return top;
    return path.dirname(path.resolve(gitdir, '..', '..'));
  } catch {
    return top;
  }
}

/** The cache directory: LF_FIREFOX_CACHE, or <main checkout>/tools/firefox. */
export function defaultCacheRoot({ env = process.env, top } = {}) {
  if (env.LF_FIREFOX_CACHE) return path.resolve(env.LF_FIREFOX_CACHE);
  return path.join(mainCheckout(top), 'tools', 'firefox');
}

/** The copy of this version for that package: <cacheRoot>/<version>-<key>. */
export function copyDir(cacheRoot, pkg) {
  return path.join(cacheRoot, `${FIREFOX.version}-${pkg.key}`);
}

/** Whether `dir` holds a complete copy of the pinned version: { ok, reason }. */
export function checkCopy(dir, pkg, platform = process.platform) {
  let marker;
  try {
    marker = JSON.parse(fs.readFileSync(path.join(dir, MARKER), 'utf8'));
  } catch {
    return { ok: false, reason: 'no copy' };
  }
  if (marker.version !== FIREFOX.version || marker.sha512 !== pkg.sha512) return { ok: false, reason: `another version (${marker.version})` };
  if (!fs.existsSync(appBinary(path.join(dir, 'app'), platform))) return { ok: false, reason: 'incomplete copy' };
  return { ok: true, reason: '' };
}

/** How to get a copy when there is none (appended to the error). */
export function preparationHelp(pkg, cacheRoot) {
  const lines = [
    'Firefox for npm run test:firefox: the tests never use the Firefox installed on this computer by default.',
    'Prepare their own copy in one of these ways:',
    `  - with network access: npm run firefox:prepare (downloads Firefox ${FIREFOX.version}, about 90 MB, from archive.mozilla.org into ${cacheRoot});`,
    '    behind a proxy, set HTTPS_PROXY and NODE_USE_ENV_PROXY=1 for Node;',
  ];
  if (pkg) {
    lines.push(`  - offline: put ${pkg.name} (from ${pkg.url}, SHA-512 ${pkg.sha512}) into ${path.join(cacheRoot, 'downloads')} and run npm run firefox:prepare;`);
  }
  lines.push('  - or set LF_FIREFOX=<path to a Firefox> to run that one (not a Firefox you browse with: the test cannot keep it from updating).');
  return lines.join('\n');
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function sha512File(file) {
  return new Promise((resolve, reject) => {
    const h = crypto.createHash('sha512');
    fs.createReadStream(file).on('error', reject).on('data', (b) => h.update(b)).on('end', () => resolve(h.digest('hex')));
  });
}

/**
 * Downloads `url` to `dest` and checks its SHA-512 while writing; on any
 * failure nothing is left at `dest`. `fetchImpl` is for the tests.
 */
export async function download(url, dest, sha512, { fetchImpl = globalThis.fetch, log = () => {}, timeoutMs = 20 * 60 * 1000 } = {}) {
  const part = `${dest}.${process.pid}.${crypto.randomBytes(4).toString('hex')}.part`;
  try {
    const res = await fetchImpl(url, { redirect: 'follow', signal: AbortSignal.timeout(timeoutMs) });
    if (!res.ok || !res.body) throw new Error(`HTTP ${res.status}`);
    const total = Number(res.headers.get('content-length')) || 0;
    const hash = crypto.createHash('sha512');
    const out = fs.createWriteStream(part);
    let done = 0;
    let shown = 0;
    try {
      for await (const chunk of Readable.fromWeb(res.body)) {
        hash.update(chunk);
        done += chunk.length;
        if (!out.write(chunk)) await new Promise((r) => out.once('drain', r));
        if (total && done / total >= shown + 0.2) {
          shown = Math.floor((done / total) * 5) / 5;
          log(`downloading ${Math.round(shown * 100)}% of ${Math.round(total / 1048576)} MB`);
        }
      }
    } finally {
      await new Promise((resolve, reject) => out.end((e) => (e ? reject(e) : resolve())));
    }
    const got = hash.digest('hex');
    if (got !== sha512) throw new Error(`SHA-512 mismatch: got ${got.slice(0, 16)}…, expected ${sha512.slice(0, 16)}…`);
    fs.renameSync(part, dest);
  } finally {
    fs.rmSync(part, { force: true });
  }
}

function run(cmd, args, opts = {}) {
  const r = spawnSync(cmd, args, { stdio: ['ignore', 'ignore', 'pipe'], encoding: 'utf8', windowsHide: true, timeout: 10 * 60 * 1000, ...opts });
  if (r.error) throw new Error(`${path.basename(cmd)}: ${r.error.message}`);
  if (r.status !== 0) throw new Error(`${path.basename(cmd)} exited with ${r.status}: ${String(r.stderr || '').trim().split('\n').slice(-3).join(' | ')}`);
}

/** Unpacks the package into `work` and returns the application directory in it. */
function unpack(archive, work, platform, log) {
  const x = path.join(work, 'x');
  fs.mkdirSync(x);
  if (platform === 'win32') {
    // Windows' bsdtar (not Git's GNU tar, which may come first on PATH).
    const bsdtar = path.join(process.env.SystemRoot || 'C:\\Windows', 'System32', 'tar.exe');
    try {
      if (!fs.existsSync(bsdtar)) throw new Error(`${bsdtar} not found`);
      run(bsdtar, ['-xf', archive, '-C', x, 'core']);
    } catch (e) {
      // Without LZMA in libarchive (older Windows 10): the installer's own
      // extract-only mode. It installs nothing; its temporary files go to
      // a directory of ours.
      log(`tar.exe could not unpack the installer (${e.message}); using its extract-only mode`);
      fs.rmSync(x, { recursive: true, force: true });
      const t = path.join(work, 't');
      fs.mkdirSync(t);
      run(archive, [`/ExtractDir=${x}`], { env: { ...process.env, TEMP: t, TMP: t } });
    }
    return path.join(x, 'core');
  }
  if (platform === 'darwin') {
    const mnt = path.join(work, 'mnt');
    fs.mkdirSync(mnt);
    run('hdiutil', ['attach', '-nobrowse', '-readonly', '-noautoopen', '-mountpoint', mnt, archive]);
    try {
      const app = fs.readdirSync(mnt).find((n) => /^Firefox.*\.app$/.test(n));
      if (!app) throw new Error('no Firefox application in the disk image');
      run('ditto', [path.join(mnt, app), path.join(x, 'Firefox.app')]);
    } finally {
      try {
        run('hdiutil', ['detach', mnt]);
      } catch {
        run('hdiutil', ['detach', '-force', mnt]);
      }
    }
    return x;
  }
  run('tar', ['-xJf', archive, '-C', x]);
  return path.join(x, 'firefox');
}

/** Writes the policies into the copy (only when they differ). */
function writePolicies(appDir, platform) {
  const dir = distributionDir(appDir, platform);
  const file = path.join(dir, 'policies.json');
  const text = `${JSON.stringify(POLICIES, null, 2)}\n`;
  let old = null;
  try {
    old = fs.readFileSync(file, 'utf8');
  } catch {}
  if (old === text) return;
  fs.mkdirSync(dir, { recursive: true });
  fs.writeFileSync(file, text);
}

async function renameWithRetry(from, to) {
  // Fresh executables can be held for a moment by an antivirus scan.
  for (let i = 0; ; i++) {
    try {
      fs.renameSync(from, to);
      return;
    } catch (e) {
      if (i >= 20 || !['EPERM', 'EBUSY', 'EACCES'].includes(e.code) || fs.existsSync(to)) throw e;
      await sleep(500);
    }
  }
}

/**
 * The pinned Firefox in `cacheRoot`, downloaded and unpacked if needed:
 * { binary, dir, appDir, version, downloaded, unpacked }.
 */
export async function ensureFirefox({ cacheRoot, platform = process.platform, arch = process.arch, fetchImpl, log = () => {} } = {}) {
  const pkg = packageFor(platform, arch);
  if (!pkg) throw new Error(`no Firefox package is pinned for ${platform}/${arch}`);
  const dir = copyDir(cacheRoot, pkg);
  const result = (downloaded, unpacked) => {
    const appDir = path.join(dir, 'app');
    writePolicies(appDir, platform);
    return { binary: appBinary(appDir, platform), dir, appDir, version: FIREFOX.version, downloaded, unpacked };
  };
  const have = checkCopy(dir, pkg, platform);
  if (have.ok) return result(false, false);

  const downloads = path.join(cacheRoot, 'downloads');
  fs.mkdirSync(downloads, { recursive: true });
  const archive = path.join(downloads, pkg.name);
  let fetched = false;
  if (fs.existsSync(archive) && (await sha512File(archive)) === pkg.sha512) {
    log(`using ${archive}`);
  } else {
    fs.rmSync(archive, { force: true });
    log(`downloading Firefox ${FIREFOX.version} (${pkg.key}) from ${pkg.url}`);
    await download(pkg.url, archive, pkg.sha512, { fetchImpl, log });
    fetched = true;
  }

  const work = fs.mkdtempSync(path.join(cacheRoot, '.unpack-'));
  try {
    const unpacked = unpack(archive, work, platform, log);
    const stage = path.join(work, 'copy');
    fs.mkdirSync(stage);
    const appDir = path.join(stage, 'app');
    await renameWithRetry(unpacked, appDir);
    if (!fs.existsSync(appBinary(appDir, platform))) throw new Error(`the package has no ${path.relative(appDir, appBinary(appDir, platform))}`);
    for (const f of updaterFiles(appDir, platform)) fs.rmSync(f, { recursive: true, force: true });
    writePolicies(appDir, platform);
    fs.writeFileSync(path.join(stage, MARKER), `${JSON.stringify({ version: FIREFOX.version, package: pkg.key, file: pkg.file, sha512: pkg.sha512, unpacked: new Date().toISOString() }, null, 2)}\n`);
    // Another run may have finished first: then its copy is used and ours goes.
    if (!checkCopy(dir, pkg, platform).ok) {
      if (fs.existsSync(dir)) fs.rmSync(dir, { recursive: true, force: true }); // an incomplete copy
      try {
        await renameWithRetry(stage, dir);
      } catch (e) {
        if (!checkCopy(dir, pkg, platform).ok) throw e;
      }
    }
  } finally {
    for (let i = 0; i < 10 && fs.existsSync(work); i++) {
      try {
        fs.rmSync(work, { recursive: true, force: true });
      } catch {
        await sleep(500);
      }
    }
  }
  if (fetched) fs.rmSync(archive, { force: true });
  log(`Firefox ${FIREFOX.version} unpacked into ${dir}`);
  return result(fetched, true);
}

/**
 * The Firefox the tests run: { binary, source, version, ownDirs, cacheRoot }.
 * LF_FIREFOX wins (source 'LF_FIREFOX', ownDirs [], cacheRoot null);
 * otherwise the pinned copy: source 'download', 'unpacked' (from an archive
 * put into downloads/) or 'cache'; ownDirs [its directory], by which the
 * test recognises its processes; cacheRoot, where the test also keeps its
 * temporary files (see firefox.test.mjs). Never the installed Firefox: when
 * there is no copy and none can be had, this throws with instructions.
 */
export async function firefoxForTests({ env = process.env, top, platform = process.platform, arch = process.arch, fetchImpl, log = () => {} } = {}) {
  if (env.LF_FIREFOX) {
    const bin = env.LF_FIREFOX;
    // a bare command name ("firefox") is looked up on PATH by web-ext
    if (/[\\/]/.test(bin) && !fs.existsSync(bin)) throw new Error(`LF_FIREFOX=${bin}: no such file`);
    return { binary: bin, source: 'LF_FIREFOX', version: null, ownDirs: [], cacheRoot: null };
  }
  const cacheRoot = defaultCacheRoot({ env, top });
  const pkg = packageFor(platform, arch);
  try {
    const c = await ensureFirefox({ cacheRoot, platform, arch, fetchImpl, log });
    return { binary: c.binary, source: c.downloaded ? 'download' : c.unpacked ? 'unpacked' : 'cache', version: c.version, ownDirs: [c.dir], cacheRoot };
  } catch (e) {
    throw new Error(`No Firefox for the tests: ${e.message}\n${preparationHelp(pkg, cacheRoot)}`, { cause: e });
  }
}

// npm run firefox:prepare
if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const top = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..', '..');
  try {
    const fx = await firefoxForTests({ top, env: { ...process.env, LF_FIREFOX: '' }, log: (m) => console.log(`firefox: ${m}`) });
    console.log(`Firefox ${fx.version} for npm run test:firefox: ${fx.binary} (${{ download: 'downloaded now', unpacked: 'unpacked now', cache: 'already there' }[fx.source]})`);
  } catch (e) {
    console.error(e.message);
    process.exitCode = 1;
  }
}
