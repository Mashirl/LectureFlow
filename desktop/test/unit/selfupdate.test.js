// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The in-app update's Electron side (SPEC-ext "应用内更新", M5;
// internal/selfupdate/README.md "The updates directory"): install a pending
// update at start, keep the previous version, probation of the new version,
// rollback. Everything runs in temporary directories with a recording
// stand-in for the programs (installer, open, ditto, codesign …): nothing
// is installed, no registry is touched.
'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const crypto = require('node:crypto');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');

const F = require('../../src/main/lib/updatefiles');
const {
  SelfUpdateApplier, recordingOps, startFailureReason, testInstallFrom, MAX_TRIES, RESTORE_ARG, UPDATED_ARG, HELPER_WAIT_MS,
} = require('../../src/main/selfupdate');

const sha = (b) => crypto.createHash('sha256').update(b).digest('hex');
const readJ = (p) => JSON.parse(fs.readFileSync(p, 'utf8'));
const NOW = new Date('2026-11-03T08:00:00Z');

/**
 * world({ kind, version }) → a data dir with an updates folder and an
 * installation of `version` (3.1.0) of the given kind in a temporary folder.
 */
function world({ kind = 'installer', version = '3.1.0' } = {}) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'lf-su-'));
  const dataDir = path.join(root, 'data');
  const updates = path.join(dataDir, 'updates');
  fs.mkdirSync(updates, { recursive: true });
  // the shell's profile (trust file) and the temporary folder of the copies
  const profile = path.join(root, 'profile');
  const tmp = path.join(root, 'tmp');
  fs.mkdirSync(tmp);
  fs.writeFileSync(path.join(dataDir, 'lectureflow.db'), 'db before the update');
  let install;
  if (kind === 'installer') {
    const dir = path.join(root, 'Programs', 'LectureFlow');
    fs.mkdirSync(path.join(dir, 'resources', 'bin'), { recursive: true });
    fs.writeFileSync(path.join(dir, 'LectureFlow.exe'), `electron ${version}`);
    fs.writeFileSync(path.join(dir, 'Uninstall LectureFlow.exe'), 'uninstaller');
    fs.writeFileSync(path.join(dir, 'resources', 'app.asar'), `asar ${version}`);
    fs.writeFileSync(path.join(dir, 'resources', 'bin', 'lectureflow.exe'), `service ${version}`);
    install = { platform: 'windows-x64', kind, kinds: ['installer'], target: dir, exe: 'LectureFlow.exe' };
  } else if (kind === 'portable') {
    const exe = path.join(root, 'Apps', `LectureFlow-${version}-x64-portable.exe`);
    fs.mkdirSync(path.dirname(exe), { recursive: true });
    fs.writeFileSync(exe, `portable ${version}`);
    install = { platform: 'windows-x64', kind, kinds: ['portable'], target: exe, exe: 'LectureFlow.exe' };
  } else {
    const app = path.join(root, 'Applications', 'LectureFlow.app');
    makeApp(app, version);
    install = { platform: 'darwin-arm64', kind: 'zip', kinds: ['zip', 'dmg'], target: app, exe: 'LectureFlow' };
  }
  return { root, dataDir, updates, install, version, profile, tmp };
}

/** trust(w) → the trust file in w's profile ({} when there is none). */
function trust(w) {
  const f = path.join(w.profile, F.TRUST_FILE);
  return fs.existsSync(f) ? readJ(f) : {};
}

/** isCopy(w, file, name): file is a private copy of the package name in w's temporary folder. */
function isCopy(w, file, name) {
  return path.dirname(path.dirname(file)) === w.tmp && /^lectureflow-update-\d+$/.test(path.basename(path.dirname(file))) && path.basename(file) === name;
}

const copies = (w) => fs.readdirSync(w.tmp).filter((n) => n.startsWith('lectureflow-update-'));

function makeApp(app, marker) {
  fs.mkdirSync(path.join(app, 'Contents', 'MacOS'), { recursive: true });
  fs.writeFileSync(path.join(app, 'Contents', 'Info.plist'), 'plist');
  fs.writeFileSync(path.join(app, 'Contents', 'MacOS', 'LectureFlow'), `mac ${marker}`);
}

function appMarker(app) {
  return fs.readFileSync(path.join(app, 'Contents', 'MacOS', 'LectureFlow'), 'utf8');
}

/** pkg(w, version, name, content) → the fields of an update.json package, stored in updates/<version>/. */
function pkg(w, version, name, content) {
  const dir = path.join(w.updates, version);
  fs.mkdirSync(dir, { recursive: true });
  fs.writeFileSync(path.join(dir, name), content);
  return { file: `${version}/${name}`, size: Buffer.byteLength(content), sha256: sha(Buffer.from(content)) };
}

function writePending(w, { version = '3.2.0', kind = w.install.kind, platform = w.install.platform, content, name, ...over } = {}) {
  const n = name || (kind === 'installer' ? `LectureFlow-Setup-${version}-x64.exe` : kind === 'portable' ? `LectureFlow-${version}-x64-portable.exe` : `LectureFlow-${version}-arm64-mac.${kind === 'dmg' ? 'dmg' : 'zip'}`);
  const p = pkg(w, version, n, content || `${kind} package ${version}`);
  const pending = {
    schema: 1, version, from_version: w.version, channel: 'stable', platform, kind, ...p,
    manifest: `${version}/update.json`, signature: `${version}/update.json.minisig`, key_id: '8F2D0A6B3C1E4D57', origin: 'download', verified_at: '2026-11-02T12:00:00Z',
    ...over,
  };
  fs.writeFileSync(path.join(w.updates, 'pending.json'), JSON.stringify(pending));
  return pending;
}

function writePrevious(w, { withFile = false, ...over } = {}) {
  const prev = { schema: 1, version: w.version, platform: w.install.platform, kind: w.install.kind, created_at: '2026-11-02T12:00:00Z', ...over };
  if (withFile) {
    const name = w.install.kind === 'installer' ? `LectureFlow-Setup-${w.version}-x64.exe` : w.install.kind === 'portable' ? `LectureFlow-${w.version}-x64-portable.exe` : `LectureFlow-${w.version}-arm64-mac.zip`;
    Object.assign(prev, pkg(w, w.version, name, `${w.install.kind} package ${w.version}`));
  }
  fs.writeFileSync(path.join(w.updates, 'previous.json'), JSON.stringify(prev));
  return prev;
}

/**
 * goVerifies(dir, version, { previous, copyTo }): what `lectureflow
 * -selfupdate-verify dir` answers when the Go side accepts the JSON file
 * (runVerify's result): it checks the package's bytes and, with copyTo,
 * copies it into a new folder there (pending.json, or previous.json with
 * previous).
 */
let copyNo = 0;
function goVerifies(dir, version, { previous = false, copyTo = '' } = {}) {
  const say = (code, line) => ({ code, stdout: `${line}\n`, stderr: '', timedOut: false, error: null });
  const p = readJ(path.join(dir, previous ? 'previous.json' : 'pending.json'));
  if (!p.file) return say(1, `invalid selfupdate.not_available: previous.json names no package of ${p.version}`);
  const src = path.join(dir, ...p.file.split('/'));
  if (!fs.existsSync(src)) return say(1, `invalid selfupdate.io_failed: open ${src}: no such file`);
  if (sha(fs.readFileSync(src)) !== p.sha256) return say(1, 'invalid selfupdate.checksum: the package has other bytes');
  let line = `ok version=${p.version} kind=${p.kind} file=${p.file} sha256=${p.sha256}`;
  if (copyTo) {
    copyNo += 1;
    const d = path.join(copyTo, `lectureflow-update-${process.pid}${copyNo}`);
    fs.mkdirSync(d);
    fs.copyFileSync(src, path.join(d, path.posix.basename(p.file)));
    line += ` copy=${path.join(d, path.posix.basename(p.file))}`;
  }
  return say(0, line);
}

/**
 * fakeOps: records every program start; the macOS tools are simulated on
 * the file system (a "zip" or "dmg" holds the marker of the app inside).
 * The update check (-selfupdate-verify) is recorded in `verifies`, not in
 * `calls`; verify(dir, version) → its result (default: the Go side agrees).
 */
function fakeOps({ bundleId = 'org.lectureflow.desktop', codesignFails = false, alive = () => false, verify = goVerifies } = {}) {
  const calls = [];
  const verifies = [];
  let pid = 4000;
  return {
    calls,
    verifies,
    verifyPending: async (dir, version, opts = {}) => {
      verifies.push({ dir, version, applying: fs.existsSync(path.join(dir, 'applying.json')), previous: !!opts.previous });
      return verify(dir, version, opts);
    },
    spawnDetached: async (file, args, opts = {}) => {
      calls.push({ op: 'spawn', file, args, verbatim: !!opts.verbatim });
      pid += 1;
      return pid;
    },
    run: async (file, args) => {
      calls.push({ op: 'run', file: path.basename(file), args });
      const tool = path.basename(file);
      if (tool === 'ditto' && args[0] === '-x') {
        makeApp(path.join(args[3], 'LectureFlow.app'), fs.readFileSync(args[2], 'utf8'));
      } else if (tool === 'ditto') {
        fs.cpSync(args[0], args[1], { recursive: true });
      } else if (tool === 'hdiutil' && args[0] === 'attach') {
        makeApp(path.join(args[args.indexOf('-mountpoint') + 1], 'LectureFlow.app'), fs.readFileSync(args[args.length - 1], 'utf8'));
      } else if (tool === 'plutil') {
        return { stdout: args[1] === 'CFBundleIdentifier' ? `${bundleId}\n` : '3.2.0\n' };
      } else if (tool === 'codesign' && codesignFails) {
        throw new Error('codesign failed: code object is not signed at all');
      }
      return { stdout: '' };
    },
    releaseLock: () => calls.push({ op: 'releaseLock' }),
    pidAlive: (p) => alive(p),
  };
}

function applier(w, { version = w.version, ops = fakeOps(), install = w.install, now = () => NOW } = {}) {
  const logs = [];
  const log = { info: (m) => logs.push(m), warn: (m) => logs.push(m), error: (m) => logs.push(m) };
  const a = new SelfUpdateApplier({ install, currentVersion: version, dataDir: w.dataDir, ops, log, now, privateDir: w.profile, tmpDir: w.tmp });
  return { a, ops, logs };
}

const has = (p) => fs.existsSync(p);
const U = (w, ...p) => path.join(w.updates, ...p);
const cleanup = (w) => fs.rmSync(w.root, { recursive: true, force: true });

// ---- helpers ----

test('versions compare like the Go side (semver precedence)', () => {
  assert.equal(F.compareVersions('3.2.0', '3.1.9'), 1);
  assert.equal(F.compareVersions('3.2.0', '3.2.0+build.7'), 0);
  assert.equal(F.compareVersions('3.2.0-beta.1', '3.2.0'), -1, 'a pre-release is older');
  assert.equal(F.compareVersions('3.2.0-beta.2', '3.2.0-beta.10'), -1, 'numeric identifiers compare as numbers');
  assert.equal(F.compareVersions('3.2.0-1', '3.2.0-alpha'), -1, 'numeric before alphanumeric');
  assert.equal(F.compareVersions('3.2.0-beta', '3.2.0-beta.1'), -1);
  assert.equal(F.compareVersions('3.10.0', '3.9.0'), 1);
  assert.equal(F.parseVersion('v3.1.0'), null, 'no "v"');
  assert.equal(F.parseVersion('3.1'), null);
  assert.equal(F.parseVersion('03.1.0'), null);
  assert.equal(F.sameVersion('3.1.0', '3.1.0'), true);
  assert.equal(F.sameVersion('dev', 'dev'), false, 'only valid versions are the same');
});

test('paths in the JSON files stay inside the updates folder', () => {
  const dir = path.join(os.tmpdir(), 'u');
  assert.equal(F.resolveInside(dir, '3.2.0/a.exe'), path.join(dir, '3.2.0', 'a.exe'));
  for (const bad of ['', '/etc/passwd', 'C:/x', 'c:x', '..', '../x', '3.2.0/../../x', '3.2.0\\a.exe', '\\\\server\\x', 'a\0b']) {
    assert.equal(F.resolveInside(dir, bad), null, bad);
  }
});

test('what this installation is', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'lf-inst-'));
  try {
    const dir = path.join(root, 'LectureFlow');
    fs.mkdirSync(path.join(dir, 'resources'), { recursive: true });
    const exe = path.join(dir, 'LectureFlow.exe');
    const win = (env = {}) => F.describeInstallation({ isPackaged: true, platform: 'win32', arch: 'x64', execPath: exe, env });
    assert.equal(F.describeInstallation({ isPackaged: false, platform: 'win32', arch: 'x64', execPath: exe }), null, 'development: nothing');
    assert.match(win().unsupported, /not set up by its installer/, 'an unpacked folder is not an installation');
    fs.writeFileSync(path.join(dir, 'Uninstall LectureFlow.exe'), '');
    assert.deepEqual(win(), { platform: 'windows-x64', kind: 'installer', kinds: ['installer'], target: dir, exe: 'LectureFlow.exe' });
    assert.deepEqual(win({ PORTABLE_EXECUTABLE_FILE: 'D:\\Apps\\LF-portable.exe' }), {
      platform: 'windows-x64', kind: 'portable', kinds: ['portable'], target: 'D:\\Apps\\LF-portable.exe', exe: 'LectureFlow.exe',
    });
    assert.ok(win({ PORTABLE_EXECUTABLE_FILE: 'relative.exe' }).unsupported);
    const mac = F.describeInstallation({ isPackaged: true, platform: 'darwin', arch: 'arm64', execPath: '/Applications/LectureFlow.app/Contents/MacOS/LectureFlow' });
    assert.equal(mac.kind, 'zip');
    assert.deepEqual(mac.kinds, ['zip', 'dmg']);
    assert.equal(mac.target, path.resolve('/Applications/LectureFlow.app'));
    assert.ok(F.describeInstallation({ isPackaged: true, platform: 'darwin', arch: 'x64', execPath: '/Applications/LectureFlow.app/Contents/MacOS/LectureFlow' }).unsupported);
    assert.ok(F.describeInstallation({ isPackaged: true, platform: 'linux', arch: 'x64', execPath: '/opt/lf/lectureflow' }).unsupported);
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
});

test('NSIS arguments: silent, per user, start the app, /D= last and unquoted', () => {
  assert.deepEqual(F.nsisArguments('C:\\Users\\A B\\AppData\\Local\\Programs\\LectureFlow'), [
    '--updated', '/S', '--force-run', '/D=C:\\Users\\A B\\AppData\\Local\\Programs\\LectureFlow',
  ]);
});

test('startFailureReason and the test hook', () => {
  assert.equal(startFailureReason({ kind: 'timeout' }), 'the service did not answer within 30 seconds');
  assert.equal(startFailureReason({ kind: 'exited', code: 1, reason: 'migration 011: no such table' }), 'the service stopped while starting (exit code 1): migration 011: no such table');
  assert.match(startFailureReason(new Error('boom')), /boom/);
  const hook = JSON.stringify({ install: { platform: 'windows-x64', kind: 'portable', target: path.resolve('x.exe') }, mark: path.resolve('mark.jsonl') });
  assert.equal(testInstallFrom({ LECTUREFLOW_TEST_SELFUPDATE: hook }, true), null, 'never in a packaged app');
  assert.equal(testInstallFrom({ LECTUREFLOW_TEST_SELFUPDATE: hook }, false).install.kind, 'portable');
  assert.equal(testInstallFrom({ LECTUREFLOW_TEST_SELFUPDATE: '{"install":{"kind":"evil"},"mark":"/m"}' }, false), null);
});

// ---- nothing to do ----

test('development (no installation) touches nothing', async () => {
  const w = world();
  try {
    const pending = writePending(w);
    const { a, ops } = applier(w, { install: null });
    assert.deepEqual(await a.atStart(), { action: 'continue' });
    assert.ok(has(U(w, 'pending.json')));
    assert.deepEqual(ops.calls, []);
    assert.equal(pending.version, '3.2.0');
  } finally {
    cleanup(w);
  }
});

test('no pending.json: continue', async () => {
  const w = world();
  try {
    const { a, ops } = applier(w);
    assert.deepEqual(await a.atStart(), { action: 'continue' });
    assert.deepEqual(ops.calls, []);
    assert.equal(has(U(w, 'result.json')), false);
  } finally {
    cleanup(w);
  }
});

// ---- pending.json checks ----

test('a pending update that is not newer is dropped silently (it was installed)', async () => {
  const w = world({ version: '3.2.0' });
  try {
    writePending(w, { version: '3.2.0' });
    const { a, ops } = applier(w);
    assert.equal((await a.atStart()).action, 'continue');
    assert.equal(has(U(w, 'pending.json')), false);
    assert.equal(has(U(w, 'result.json')), false);
    assert.deepEqual(ops.calls, []);
  } finally {
    cleanup(w);
  }
});

for (const [name, over, expect] of [
  ['another schema', { schema: 2 }, /schema 2/],
  ['another platform', { platform: 'darwin-arm64' }, /for darwin-arm64/],
  ['another kind', { kind: 'portable' }, /needs a "installer" package, not "portable"/],
  ['a path outside the updates folder', { file: '../outside.exe' }, /outside the updates folder/],
  ['a damaged package', { sha256: 'a'.repeat(64) }, /damaged \(SHA-256 mismatch\)/],
  ['a package of another size', { size: 5 }, /has \d+ bytes, not 5/],
  ['a missing package', { file: '3.2.0/missing.exe' }, /missing/],
  ['no valid hash', { sha256: 'XYZ' }, /no valid size or SHA-256/],
]) {
  test(`pending.json with ${name} is dropped and reported`, async () => {
    const w = world();
    try {
      writePending(w, over);
      writePrevious(w);
      const { a, ops } = applier(w);
      assert.equal((await a.atStart()).action, 'continue');
      assert.equal(has(U(w, 'pending.json')), false);
      const r = readJ(U(w, 'result.json'));
      assert.equal(r.outcome, 'failed');
      assert.equal(r.version, '3.2.0');
      assert.equal(r.from, '3.1.0');
      assert.match(r.error, expect);
      assert.deepEqual(ops.calls, [], 'nothing was started');
      assert.equal(has(U(w, 'applying.json')), false);
    } finally {
      cleanup(w);
    }
  });
}

test('a copy that cannot update itself reports it and drops the pending update', async () => {
  const w = world();
  try {
    writePending(w);
    const { a, ops } = applier(w, { install: { unsupported: 'this copy of LectureFlow was not set up by its installer; install the new version by hand' } });
    assert.equal((await a.atStart()).action, 'continue');
    assert.equal(has(U(w, 'pending.json')), false);
    assert.match(readJ(U(w, 'result.json')).error, /install the new version by hand/);
    assert.deepEqual(ops.calls, []);
  } finally {
    cleanup(w);
  }
});

test('a start in the background (browser extension) leaves the pending update for later', async () => {
  const w = world();
  try {
    writePending(w);
    writePrevious(w);
    const { a, ops } = applier(w);
    assert.equal((await a.atStart({ background: true })).action, 'continue');
    assert.ok(has(U(w, 'pending.json')));
    assert.deepEqual(ops.calls, []);
  } finally {
    cleanup(w);
  }
});

test('previous.json that does not describe the running version: not installed', async () => {
  for (const prev of [null, { version: '3.0.0' }, { platform: 'darwin-arm64' }]) {
    const w = world();
    try {
      writePending(w);
      if (prev) writePrevious(w, prev);
      const { a, ops } = applier(w);
      assert.equal((await a.atStart()).action, 'continue');
      assert.equal(has(U(w, 'pending.json')), false);
      assert.match(readJ(U(w, 'result.json')).error, /previous\.json does not describe the running version/);
      assert.deepEqual(ops.calls, []);
      assert.equal(fs.readFileSync(path.join(w.install.target, 'LectureFlow.exe'), 'utf8'), 'electron 3.1.0');
    } finally {
      cleanup(w);
    }
  }
});

// ---- the service program's check (-selfupdate-verify) ----

test('the service program checks the pending update before anything is backed up or started', async () => {
  for (const kind of ['installer', 'portable', 'zip']) {
    const w = world({ kind });
    try {
      writePending(w);
      writePrevious(w);
      const seen = [];
      const ops = fakeOps({
        verify: (dir, version, opts) => {
          seen.push({ backup: has(U(w, 'backup')), pending: has(U(w, 'pending.json')), copyTo: opts.copyTo });
          return goVerifies(dir, version, opts);
        },
      });
      const { a, logs } = applier(w, { ops });
      assert.equal((await a.atStart()).action, 'exit', kind);
      assert.deepEqual(ops.verifies, [{ dir: w.updates, version: '3.1.0', applying: false, previous: false }], `${kind}: once, on the updates folder, with the running version`);
      assert.deepEqual(seen, [{ backup: false, pending: true, copyTo: w.tmp }], `${kind}: before the backup and before pending.json is consumed; a private copy in the temporary folder`);
      assert.ok(ops.calls.length > 0, `${kind}: installed after the check`);
      assert.ok(logs.some((l) => /the service program verified 3\.2\.0/.test(l)));
      // only the installer still runs from its copy (a later start removes it)
      assert.equal(copies(w).length, kind === 'installer' ? 1 : 0, `${kind}: copies left ${copies(w)}`);
    } finally {
      cleanup(w);
    }
  }
});

test('the check says "invalid" (exit 1): not installed, pending.json removed and reported, nothing else touched', async () => {
  const w = world();
  try {
    const pending = writePending(w);
    const prev = writePrevious(w);
    const stdout = 'invalid selfupdate.signature_invalid: update.json.minisig does not verify (https://mirror.example/u.json?token=SECRET#frag)\n';
    const { a, ops, logs } = applier(w, { ops: fakeOps({ verify: () => ({ code: 1, stdout, stderr: '', timedOut: false, error: null }) }) });
    assert.deepEqual(await a.atStart(), { action: 'continue' });
    assert.equal(has(U(w, 'pending.json')), false);
    const r = readJ(U(w, 'result.json'));
    assert.deepEqual({ version: r.version, from: r.from, outcome: r.outcome }, { version: '3.2.0', from: '3.1.0', outcome: 'failed' });
    assert.match(r.error, /selfupdate\.signature_invalid/);
    assert.match(r.error, /https:\/\/mirror\.example\/u\.json/);
    for (const text of [r.error, ...logs]) assert.ok(!/SECRET|token=|#frag/.test(text), `no query string: ${text}`);
    assert.deepEqual(ops.calls, [], 'nothing was started');
    assert.equal(has(U(w, 'applying.json')), false);
    assert.equal(has(U(w, 'backup')), false, 'nothing backed up');
    assert.ok(has(U(w, pending.file)), 'the package is left to the service');
    assert.deepEqual(readJ(U(w, 'previous.json')), prev);
  } finally {
    cleanup(w);
  }
});

for (const [name, change] of [
  ['another version', (p) => ({ ...p, version: '3.3.0' })],
  ['another kind', (p) => ({ ...p, kind: 'portable' })],
  ['another file', (p) => ({ ...p, file: '3.2.0/other.exe' })],
  ['another SHA-256', (p) => ({ ...p, sha256: 'f'.repeat(64) })],
]) {
  test(`the check vouches for ${name}: the pending update is dropped`, async () => {
    const w = world();
    try {
      writePending(w);
      writePrevious(w);
      const verify = (dir) => {
        const p = change(readJ(path.join(dir, 'pending.json')));
        return { code: 0, stdout: `ok version=${p.version} kind=${p.kind} file=${p.file} sha256=${p.sha256}\n`, stderr: '', timedOut: false, error: null };
      };
      const { a, ops } = applier(w, { ops: fakeOps({ verify }) });
      assert.equal((await a.atStart()).action, 'continue');
      assert.equal(has(U(w, 'pending.json')), false);
      assert.match(readJ(U(w, 'result.json')).error, /the update check vouched for .*, not the pending 3\.2\.0/);
      assert.deepEqual(ops.calls, []);
      assert.equal(has(U(w, 'backup')), false);
    } finally {
      cleanup(w);
    }
  });
}

for (const [name, run, expect] of [
  ['a build without a public key (exit 2)', { code: 2, stdout: 'not_configured: this build has no update public key\n' }, /no update public key/],
  ['a timeout', { timedOut: true }, /did not finish within 120 seconds/],
  ['other accounts can change the data folder (exit 3)', { code: 3, stdout: 'untrusted_dir: other accounts of this computer can change D:\\LF (Authenticated Users)\n' },
    /other accounts of this computer can change the data folder.*Authenticated Users/],
  ['exit 0 without the private copy', { code: 0, stdout: `ok version=3.2.0 kind=installer file=3.2.0/LectureFlow-Setup-3.2.0-x64.exe sha256=${sha(Buffer.from('installer package 3.2.0'))}\n` },
    /did not copy the package where it was asked to/],
  ['a program that cannot be run', { error: 'spawn C:\\x\\lectureflow.exe ENOENT' }, /could not run: spawn .*ENOENT/],
  ['an older program without the flag (exit 1, no "invalid" line)', { code: 1, stdout: '', stderr: 'ERROR: flag provided but not defined: -selfupdate-verify\n' }, /no usable answer \(exit code 1\): ERROR: flag provided but not defined/],
  ['exit 0 without the "ok" line', { code: 0, stdout: 'ok\n' }, /no usable answer \(exit code 0\)/],
  ['exit 0 with more than one line', { code: 0, stdout: `hello\nok version=3.2.0 kind=installer file=3.2.0/x.exe sha256=${'a'.repeat(64)}\n` }, /no usable answer/],
  ['another exit code', { code: 3, stdout: 'invalid x: y\n' }, /no usable answer \(exit code 3\)/],
  ['a check that throws', new Error('boom'), /could not run: boom/],
]) {
  test(`no verdict from the check (${name}): not installed, pending.json kept`, async () => {
    const w = world();
    try {
      writePending(w);
      const prev = writePrevious(w);
      const verify = () => {
        if (run instanceof Error) throw run;
        return { code: null, stdout: '', stderr: '', timedOut: false, error: null, ...run };
      };
      const { a, ops, logs } = applier(w, { ops: fakeOps({ verify }) });
      assert.deepEqual(await a.atStart(), { action: 'continue' });
      assert.ok(has(U(w, 'pending.json')), 'kept for a later start');
      const r = readJ(U(w, 'result.json'));
      assert.equal(r.outcome, 'failed');
      assert.match(r.error, expect);
      assert.match(r.error, /not installed at this start$/);
      assert.ok(logs.some((l) => /pending\.json is kept/.test(l)));
      assert.deepEqual(ops.calls, []);
      assert.equal(has(U(w, 'applying.json')), false);
      assert.equal(has(U(w, 'backup')), false);
      assert.deepEqual(readJ(U(w, 'previous.json')), prev);
    } finally {
      cleanup(w);
    }
  });
}

test('without an update check program nothing is installed', async () => {
  const w = world();
  try {
    writePending(w);
    writePrevious(w);
    const ops = fakeOps();
    delete ops.verifyPending;
    const { a } = applier(w, { ops });
    assert.equal((await a.atStart()).action, 'continue');
    assert.ok(has(U(w, 'pending.json')));
    assert.match(readJ(U(w, 'result.json')).error, /no update check program/);
    assert.deepEqual(ops.calls, []);
  } finally {
    cleanup(w);
  }
});

test('no check for a start in the background or a pending update that fails the shell\'s own checks', async () => {
  const w = world();
  try {
    writePending(w);
    writePrevious(w);
    const bg = applier(w);
    await bg.a.atStart({ background: true });
    assert.deepEqual(bg.ops.verifies, []);
    writePending(w, { sha256: 'a'.repeat(64) });
    const bad = applier(w);
    await bad.a.atStart();
    assert.deepEqual(bad.ops.verifies, [], 'a damaged package is dropped before the program is run');
  } finally {
    cleanup(w);
  }
});

test('the shell runs the installed service program for the check (main.js wiring)', async () => {
  const src = fs.readFileSync(path.join(__dirname, '..', '..', 'src', 'main', 'main.js'), 'utf8');
  assert.match(src, /electronOps\(\{ app, verifyPending \}\)/);
  assert.match(src, /recordingOps\(hook\.mark, \{ verifyPending \}\)/);
  assert.match(src, /\(updatesDir, version, opts\) => runVerify\(\{ \.\.\.cmd, env: process\.env \}, updatesDir, version, opts\)/);
  // The test hook records the check, then really runs it.
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'lf-rec-'));
  try {
    const mark = path.join(root, 'mark.jsonl');
    const ops = recordingOps(mark, { verifyPending: async (dir, version) => ({ code: 0, stdout: `${dir} ${version}` }) });
    assert.deepEqual(await ops.verifyPending('D:\\data\\updates', '3.1.0'), { code: 0, stdout: 'D:\\data\\updates 3.1.0' });
    const lines = fs.readFileSync(mark, 'utf8').trim().split('\n').map((l) => JSON.parse(l));
    assert.deepEqual(lines.map(({ op, dir, version }) => ({ op, dir, version })), [{ op: 'verify', dir: 'D:\\data\\updates', version: '3.1.0' }]);
    assert.match((await recordingOps(mark).verifyPending('x', '1.0.0')).error, /no update check program/);
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
});

// ---- installing: Windows installer ----

test('installer: backs up the installation and the database, then runs NSIS silently for this user', async () => {
  const w = world();
  try {
    const pending = writePending(w);
    const prev = writePrevious(w);
    const { a, ops } = applier(w);
    const r = await a.atStart();
    assert.deepEqual(r, { action: 'exit', version: '3.2.0' });

    // The backup: a full copy (app.asar copied as a file), marked complete.
    const b = U(w, 'backup', '3.1.0');
    assert.equal(fs.readFileSync(path.join(b, 'app', 'resources', 'app.asar'), 'utf8'), 'asar 3.1.0');
    assert.equal(fs.readFileSync(path.join(b, 'app', 'LectureFlow.exe'), 'utf8'), 'electron 3.1.0');
    assert.deepEqual(readJ(path.join(b, 'backup.json')), { schema: 1, version: '3.1.0', kind: 'installer', item: '.', created_at: NOW.toISOString() });
    assert.equal(fs.readFileSync(path.join(b, 'db', 'lectureflow.db'), 'utf8'), 'db before the update');
    // previous.json: Electron adds "backup" and keeps every other field.
    assert.deepEqual(readJ(U(w, 'previous.json')), { ...prev, backup: 'backup/3.1.0' });

    // pending.json became applying.json with the bookkeeping.
    assert.equal(has(U(w, 'pending.json')), false);
    const ap = readJ(U(w, 'applying.json'));
    assert.deepEqual(ap, {
      ...pending, phase: 'installing', tries: 0, target: w.install.target, exe: 'LectureFlow.exe', started_at: NOW.toISOString(),
      db_snapshot: 'backup/3.1.0/db', pid: 4001, pid_at: NOW.toISOString(),
    });

    // The installer: verbatim arguments, /D= last; it closes this app
    // itself. It runs from the service program's private copy (M3).
    assert.equal(ops.calls.length, 1);
    assert.deepEqual({ ...ops.calls[0], file: undefined }, {
      op: 'spawn', file: undefined, args: ['--updated', '/S', '--force-run', `/D=${w.install.target}`], verbatim: true,
    });
    assert.ok(isCopy(w, ops.calls[0].file, 'LectureFlow-Setup-3.2.0-x64.exe'), ops.calls[0].file);
    assert.equal(fs.readFileSync(ops.calls[0].file, 'utf8'), 'installer package 3.2.0');
    assert.equal(has(U(w, 'result.json')), false, 'no result before the new version has started');

    // The trust file in the profile: the installation this shell started and
    // the backup it made (with the hash of its tree).
    const t = trust(w);
    assert.deepEqual(t.install, { version: '3.2.0', from_version: '3.1.0', target: w.install.target, kind: 'installer', started_at: NOW.toISOString() });
    const tree = await F.treeHash(path.join(b, 'app'));
    assert.deepEqual(t.backups['3.1.0'], {
      dir: 'backup/3.1.0', kind: 'installer', item: '.', tree: tree.sha256, files: tree.files, bytes: tree.bytes, created_at: NOW.toISOString(),
    });
  } finally {
    cleanup(w);
  }
});

test('installer: with the previous installer kept by the updater, no copy of the installation is made', async () => {
  const w = world();
  try {
    writePending(w);
    const prev = writePrevious(w, { withFile: true });
    const { a, ops } = applier(w);
    assert.equal((await a.atStart()).action, 'exit');
    assert.equal(has(U(w, 'backup', '3.1.0', 'app')), false);
    assert.deepEqual(readJ(U(w, 'previous.json')), prev, 'previous.json untouched');
    assert.ok(has(U(w, 'backup', '3.1.0', 'db', 'lectureflow.db')), 'the database is still kept');
    assert.equal(ops.calls.length, 1);
  } finally {
    cleanup(w);
  }
});

test('installer: a complete backup of the same version is reused', async () => {
  const w = world();
  try {
    writePending(w);
    writePrevious(w);
    await applier(w).a.atStart();
    // The installer did not run (e.g. the computer was switched off):
    // the old version reports it, then a new pending update comes.
    fs.writeFileSync(path.join(w.install.target, 'LectureFlow.exe'), 'electron 3.1.0');
    const old = applier(w);
    await old.a.atStart();
    assert.equal(readJ(U(w, 'result.json')).outcome, 'failed');
    fs.writeFileSync(path.join(w.install.target, 'marker'), 'not copied again');
    writePending(w);
    await applier(w).a.atStart();
    assert.equal(has(U(w, 'backup', '3.1.0', 'app', 'marker')), false, 'the existing complete backup was kept');
    assert.equal(has(U(w, 'applying.json')), true);
  } finally {
    cleanup(w);
  }
});

test('installer: a failing backup leaves everything as it was', async () => {
  const w = world();
  try {
    writePending(w);
    writePrevious(w);
    // backup/3.1.0 cannot be created: a file is in the way.
    fs.writeFileSync(U(w, 'backup'), 'not a directory');
    const { a, ops } = applier(w);
    assert.equal((await a.atStart()).action, 'continue');
    const r = readJ(U(w, 'result.json'));
    assert.equal(r.outcome, 'failed');
    assert.match(r.error, /could not back up LectureFlow 3\.1\.0/);
    assert.equal(has(U(w, 'pending.json')), false);
    assert.equal(has(U(w, 'applying.json')), false);
    assert.deepEqual(ops.calls, []);
  } finally {
    cleanup(w);
  }
});

test('installer: the installer that cannot be started changes nothing', async () => {
  const w = world();
  try {
    writePending(w);
    writePrevious(w, { withFile: true });
    const ops = fakeOps();
    ops.spawnDetached = async () => {
      throw Object.assign(new Error('spawn EACCES'), { code: 'EACCES' });
    };
    const { a } = applier(w, { ops });
    assert.equal((await a.atStart()).action, 'continue');
    const r = readJ(U(w, 'result.json'));
    assert.equal(r.outcome, 'failed');
    assert.match(r.error, /could not install 3\.2\.0: spawn EACCES/);
    assert.equal(has(U(w, 'applying.json')), false);
    assert.equal(has(U(w, 'backup', '3.1.0', 'db')), false, 'the database snapshot is dropped');
  } finally {
    cleanup(w);
  }
});

// ---- installing: portable ----

test('portable: the executable is replaced (the running one renamed aside) and the new one started', async () => {
  const w = world({ kind: 'portable' });
  try {
    writePending(w, { content: 'portable 3.2.0' });
    writePrevious(w);
    const { a, ops } = applier(w);
    assert.equal((await a.atStart()).action, 'exit');
    assert.equal(fs.readFileSync(w.install.target, 'utf8'), 'portable 3.2.0');
    assert.equal(fs.readFileSync(`${w.install.target}.lf-old`, 'utf8'), 'portable 3.1.0');
    assert.equal(has(`${w.install.target}.lf-new`), false);
    const item = path.basename(w.install.target);
    assert.equal(fs.readFileSync(U(w, 'backup', '3.1.0', 'app', item), 'utf8'), 'portable 3.1.0');
    assert.equal(readJ(U(w, 'backup', '3.1.0', 'backup.json')).item, item);
    assert.equal(readJ(U(w, 'previous.json')).backup, 'backup/3.1.0');
    // The lock is released before the new version starts.
    assert.deepEqual(ops.calls, [{ op: 'releaseLock' }, { op: 'spawn', file: w.install.target, args: [UPDATED_ARG], verbatim: false }]);
    assert.equal(readJ(U(w, 'applying.json')).phase, 'installing');
  } finally {
    cleanup(w);
  }
});

test('portable: a replacement that fails leaves the executable as it was', async () => {
  const w = world({ kind: 'portable' });
  try {
    writePending(w);
    writePrevious(w, { withFile: true });
    // Something occupies the name the new executable is copied to.
    fs.mkdirSync(`${w.install.target}.lf-new`);
    fs.writeFileSync(path.join(`${w.install.target}.lf-new`, 'x'), 'x');
    const { a, ops } = applier(w);
    // The leftover cleanup at start removes it; make it come back.
    const orig = a._cleanupLeftovers.bind(a);
    a._cleanupLeftovers = async () => {
      await orig();
      fs.mkdirSync(`${w.install.target}.lf-new`, { recursive: true });
      fs.writeFileSync(path.join(`${w.install.target}.lf-new`, 'x'), 'x');
    };
    assert.equal((await a.atStart()).action, 'continue');
    assert.equal(fs.readFileSync(w.install.target, 'utf8'), 'portable 3.1.0');
    assert.equal(readJ(U(w, 'result.json')).outcome, 'failed');
    assert.equal(has(U(w, 'applying.json')), false);
    assert.deepEqual(ops.calls, []);
  } finally {
    cleanup(w);
  }
});

test('a folder that cannot be written (read-only stick, app started from the dmg) gives a readable reason', async () => {
  const p = world({ kind: 'portable' });
  const m = world({ kind: 'zip' });
  const { copyFile, mkdir } = F.fsp;
  try {
    F.fsp.copyFile = async (src, dst) => {
      if (String(dst).endsWith('.lf-new')) throw Object.assign(new Error('EACCES: permission denied'), { code: 'EACCES' });
      return copyFile(src, dst);
    };
    F.fsp.mkdir = async (dir, opts) => {
      if (/\.lf-new-/.test(path.basename(String(dir)))) throw Object.assign(new Error('EROFS: read-only file system'), { code: 'EROFS' });
      return mkdir(dir, opts);
    };
    writePending(p);
    writePrevious(p, { withFile: true });
    assert.equal((await applier(p).a.atStart()).action, 'continue');
    assert.match(readJ(path.join(p.updates, 'result.json')).error, /the folder of LectureFlow-3\.1\.0-x64-portable\.exe cannot be written \(EACCES\); move LectureFlow/);
    assert.equal(fs.readFileSync(p.install.target, 'utf8'), 'portable 3.1.0');

    writePending(m, { content: '3.2.0' });
    writePrevious(m);
    assert.equal((await applier(m).a.atStart()).action, 'continue');
    assert.match(readJ(path.join(m.updates, 'result.json')).error, /the folder of LectureFlow\.app cannot be written \(EROFS\)/);
    assert.equal(appMarker(m.install.target), 'mac 3.1.0');
  } finally {
    F.fsp.copyFile = copyFile;
    F.fsp.mkdir = mkdir;
    cleanup(p);
    cleanup(m);
  }
});

// ---- installing: macOS ----

test('macOS zip: unpacked next to the app, checked, swapped; the old app becomes the backup', async () => {
  const w = world({ kind: 'zip' });
  try {
    writePending(w, { content: '3.2.0' });
    writePrevious(w);
    const { a, ops } = applier(w);
    assert.equal((await a.atStart()).action, 'exit');
    assert.equal(appMarker(w.install.target), 'mac 3.2.0');
    assert.equal(appMarker(U(w, 'backup', '3.1.0', 'app', 'LectureFlow.app')), 'mac 3.1.0');
    assert.deepEqual(readJ(U(w, 'backup', '3.1.0', 'backup.json')), { schema: 1, version: '3.1.0', kind: 'zip', item: 'LectureFlow.app', created_at: NOW.toISOString() });
    assert.equal(readJ(U(w, 'previous.json')).backup, 'backup/3.1.0');
    const tools = ops.calls.map((c) => (c.op === 'run' ? c.file : c.op));
    assert.deepEqual(tools, ['ditto', 'plutil', 'plutil', 'xattr', 'codesign', 'releaseLock', 'spawn']);
    assert.deepEqual(ops.calls.find((c) => c.file === 'xattr').args.slice(0, 2), ['-dr', 'com.apple.quarantine']);
    assert.deepEqual(ops.calls.at(-1), { op: 'spawn', file: '/usr/bin/open', args: ['-n', w.install.target, '--args', UPDATED_ARG], verbatim: false });
    const left = fs.readdirSync(path.dirname(w.install.target));
    assert.deepEqual(left, ['LectureFlow.app'], 'no staging folder left');
  } finally {
    cleanup(w);
  }
});

test('macOS dmg (offline update): mounted, copied, detached', async () => {
  const w = world({ kind: 'zip' });
  try {
    writePending(w, { kind: 'dmg', content: '3.2.0' });
    writePrevious(w);
    const { a, ops } = applier(w);
    assert.equal((await a.atStart()).action, 'exit');
    assert.equal(appMarker(w.install.target), 'mac 3.2.0');
    const hd = ops.calls.filter((c) => c.file === 'hdiutil').map((c) => c.args[0]);
    assert.deepEqual(hd, ['attach', 'detach']);
    assert.ok(ops.calls.find((c) => c.file === 'hdiutil').args.includes('-readonly'));
  } finally {
    cleanup(w);
  }
});

test('macOS: another app in the package or a bad signature is refused; nothing changes', async () => {
  for (const opts of [{ bundleId: 'com.example.other' }, { codesignFails: true }]) {
    const w = world({ kind: 'zip' });
    try {
      writePending(w, { content: '3.2.0' });
      writePrevious(w);
      const { a, ops } = applier(w, { ops: fakeOps(opts) });
      assert.equal((await a.atStart()).action, 'continue');
      assert.equal(appMarker(w.install.target), 'mac 3.1.0');
      const r = readJ(U(w, 'result.json'));
      assert.equal(r.outcome, 'failed');
      assert.match(r.error, opts.bundleId ? /holds com\.example\.other, not LectureFlow/ : /codesign failed/);
      assert.deepEqual(fs.readdirSync(path.dirname(w.install.target)), ['LectureFlow.app']);
      assert.equal(ops.calls.some((c) => c.op === 'spawn'), false);
    } finally {
      cleanup(w);
    }
  }
});

// ---- the new version starts ----

/** installed(w, opts) → a world where 3.1.0 installed 3.2.0 (installer kind unless given). */
async function installed({ kind = 'installer', withFile = false } = {}) {
  const w = world({ kind });
  writePending(w, { content: kind === 'zip' ? '3.2.0' : `${kind} package 3.2.0` });
  writePrevious(w, { withFile });
  const first = applier(w);
  assert.equal((await first.a.atStart()).action, 'exit');
  // What the installer does: the new version is in place; it touches the database.
  if (kind === 'installer') fs.writeFileSync(path.join(w.install.target, 'LectureFlow.exe'), 'electron 3.2.0');
  fs.writeFileSync(path.join(w.dataDir, 'lectureflow.db'), 'db migrated by 3.2.0');
  fs.writeFileSync(path.join(w.dataDir, 'lectureflow.db-wal'), 'wal of 3.2.0');
  return w;
}

test('new version: probation, then healthy → installed', async () => {
  const w = await installed();
  try {
    const { a, ops } = applier(w, { version: '3.2.0' });
    assert.deepEqual(await a.atStart(), { action: 'probation', version: '3.2.0', tries: 1 });
    assert.equal(readJ(U(w, 'applying.json')).tries, 1);
    assert.equal(readJ(U(w, 'applying.json')).pid, undefined);
    // Written before the service starts, so the service can show it.
    assert.equal(readJ(U(w, 'result.json')).outcome, 'installed');
    assert.equal(a.inProbation, true);
    await a.healthy();
    assert.equal(a.inProbation, false);
    assert.equal(has(U(w, 'applying.json')), false);
    assert.deepEqual(readJ(U(w, 'result.json')), { version: '3.2.0', from: '3.1.0', outcome: 'installed', at: NOW.toISOString() });
    assert.equal(has(U(w, 'backup', '3.1.0', 'db')), false, 'the database snapshot only serves the first start');
    assert.ok(has(U(w, 'backup', '3.1.0', 'app', 'LectureFlow.exe')), 'the previous version stays');
    assert.equal(fs.readFileSync(path.join(w.dataDir, 'lectureflow.db'), 'utf8'), 'db migrated by 3.2.0');
    assert.deepEqual(ops.calls, []);
  } finally {
    cleanup(w);
  }
});

test('new version: one failed start is retried, the second puts 3.1.0 back (installer kept by the updater)', async () => {
  const w = await installed({ withFile: true });
  try {
    let s = applier(w, { version: '3.2.0' });
    assert.equal((await s.a.atStart()).action, 'probation');
    assert.deepEqual(await s.a.failed('the service did not answer within 30 seconds'), { action: 'retry' });
    assert.equal(readJ(U(w, 'applying.json')).error, 'the service did not answer within 30 seconds');
    assert.equal(fs.readFileSync(path.join(w.dataDir, 'lectureflow.db'), 'utf8'), 'db migrated by 3.2.0', 'not yet');

    // The shell starts the service once more.
    assert.deepEqual(await s.a.atStart(), { action: 'probation', version: '3.2.0', tries: 2 });
    const r = await s.a.failed('the service stopped while starting (exit code 1): migration 011: boom');
    assert.deepEqual(r, { action: 'exit' });

    // The database as it was before the update; the new WAL is gone.
    assert.equal(fs.readFileSync(path.join(w.dataDir, 'lectureflow.db'), 'utf8'), 'db before the update');
    assert.equal(has(path.join(w.dataDir, 'lectureflow.db-wal')), false);
    // The previous installer, silently, into the same folder: vouched for by
    // the service program (-selfupdate-previous) and run from its private
    // copy (sec-update M2).
    assert.equal(s.ops.calls.length, 1);
    assert.deepEqual({ ...s.ops.calls[0], file: undefined }, {
      op: 'spawn', file: undefined, args: ['--updated', '/S', '--force-run', `/D=${w.install.target}`], verbatim: true,
    });
    assert.ok(isCopy(w, s.ops.calls[0].file, 'LectureFlow-Setup-3.1.0-x64.exe'), s.ops.calls[0].file);
    assert.deepEqual(s.ops.verifies.map((x) => x.previous), [true]);
    const ap = readJ(U(w, 'applying.json'));
    assert.equal(ap.phase, 'rolling_back');
    assert.equal(ap.restore, 'file');
    assert.equal(ap.error, 'the service stopped while starting (exit code 1): migration 011: boom');

    // 3.1.0 starts again: rolled_back with the reason.
    s = applier(w, { version: '3.1.0' });
    assert.equal((await s.a.atStart()).action, 'continue');
    assert.deepEqual(readJ(U(w, 'result.json')), {
      version: '3.2.0', from: '3.1.0', outcome: 'rolled_back', at: NOW.toISOString(), error: 'the service stopped while starting (exit code 1): migration 011: boom',
    });
    assert.equal(has(U(w, 'applying.json')), false);
    assert.equal(has(U(w, 'backup', '3.1.0', 'db')), false);
  } finally {
    cleanup(w);
  }
});

test('new version: a crash during the start counts as a try; the third start goes back at once', async () => {
  const w = await installed({ withFile: true });
  try {
    await applier(w, { version: '3.2.0' }).a.atStart(); // try 1, then the app crashed
    await applier(w, { version: '3.2.0' }).a.atStart(); // try 2, crashed again
    const s = applier(w, { version: '3.2.0' });
    assert.deepEqual(await s.a.atStart(), { action: 'exit' });
    const ap = readJ(U(w, 'applying.json'));
    assert.equal(ap.phase, 'rolling_back');
    assert.match(ap.error, new RegExp(`did not start ${MAX_TRIES} times in a row`));
    assert.equal(s.ops.calls[0].op, 'spawn');
  } finally {
    cleanup(w);
  }
});

test('installer rollback from the backup copy: the copy puts itself back', async () => {
  const w = await installed();
  try {
    const s = applier(w, { version: '3.2.0' });
    await s.a.atStart();
    await s.a.failed('first');
    await s.a.atStart();
    assert.deepEqual(await s.a.failed('the window did not load the app: ERR_FAILED (-2)'), { action: 'exit' });
    const helper = path.join(U(w, 'backup', '3.1.0', 'app'), 'LectureFlow.exe');
    assert.deepEqual(s.ops.calls, [{ op: 'releaseLock' }, { op: 'spawn', file: helper, args: [RESTORE_ARG], verbatim: false }]);
    const ap = readJ(U(w, 'applying.json'));
    assert.equal(ap.restore, 'backup');
    assert.equal(ap.pid, 4001);

    // The copy runs (as 3.1.0) and replaces the failed installation.
    const h = applier(w, { version: '3.1.0' });
    assert.equal(await h.a.restoreFromBackup({ execPath: helper }), true);
    assert.equal(fs.readFileSync(path.join(w.install.target, 'LectureFlow.exe'), 'utf8'), 'electron 3.1.0');
    assert.equal(fs.readFileSync(path.join(w.install.target, 'resources', 'app.asar'), 'utf8'), 'asar 3.1.0');
    assert.deepEqual(fs.readdirSync(path.dirname(w.install.target)), ['LectureFlow'], 'the failed installation is removed');
    assert.deepEqual(h.ops.calls, [{ op: 'releaseLock' }, { op: 'spawn', file: path.join(w.install.target, 'LectureFlow.exe'), args: [UPDATED_ARG], verbatim: false }]);
    assert.equal(readJ(U(w, 'applying.json')).pid, undefined, 'the restored app does not wait for the restore program');

    const old = applier(w, { version: '3.1.0' });
    assert.equal((await old.a.atStart()).action, 'continue');
    const r = readJ(U(w, 'result.json'));
    assert.equal(r.outcome, 'rolled_back');
    assert.equal(r.error, 'the window did not load the app: ERR_FAILED (-2)');
    assert.ok(has(U(w, 'backup', '3.1.0', 'app', 'LectureFlow.exe')), 'the backup stays');
  } finally {
    cleanup(w);
  }
});

test('the restore program refuses to run from anywhere but the recorded backup', async () => {
  const w = await installed();
  try {
    const s = applier(w, { version: '3.2.0' });
    await s.a.atStart();
    await s.a.failed('x');
    await s.a.atStart();
    await s.a.failed('y');
    const h = applier(w, { version: '3.1.0' });
    assert.equal(await h.a.restoreFromBackup({ execPath: path.join(w.root, 'elsewhere', 'LectureFlow.exe') }), false);
    assert.equal(fs.readFileSync(path.join(w.install.target, 'LectureFlow.exe'), 'utf8'), 'electron 3.2.0');
    assert.deepEqual(h.ops.calls, []);
  } finally {
    cleanup(w);
  }
});

test('a restore program that cannot replace the installation: the new version reports it', async () => {
  const w = await installed();
  try {
    const s = applier(w, { version: '3.2.0' });
    await s.a.atStart();
    await s.a.failed('x');
    await s.a.atStart();
    await s.a.failed('the service did not answer within 30 seconds');
    const helper = path.join(U(w, 'backup', '3.1.0', 'app'), 'LectureFlow.exe');
    const h = applier(w, { version: '3.1.0' });
    h.a._replaceDir = async () => {
      throw Object.assign(new Error('EBUSY: resource busy or locked'), { code: 'EBUSY' });
    };
    await h.a.restoreFromBackup({ execPath: helper });
    assert.equal(readJ(U(w, 'applying.json')).restore_error, 'EBUSY: resource busy or locked');
    // The failed version is started again and reports the whole story.
    const again = applier(w, { version: '3.2.0' });
    assert.equal((await again.a.atStart()).action, 'continue');
    const r = readJ(U(w, 'result.json'));
    assert.equal(r.outcome, 'failed');
    assert.equal(r.error, 'the service did not answer within 30 seconds; going back to 3.1.0 failed: EBUSY: resource busy or locked');
    assert.equal(has(U(w, 'applying.json')), false);
  } finally {
    cleanup(w);
  }
});

test('nothing to go back to: the failure is reported and the app keeps running', async () => {
  const w = await installed({ withFile: true });
  try {
    fs.rmSync(U(w, '3.1.0'), { recursive: true });
    const s = applier(w, { version: '3.2.0' });
    await s.a.atStart();
    await s.a.failed('x');
    await s.a.atStart();
    const r = await s.a.failed('the service did not answer within 30 seconds');
    assert.equal(r.action, 'giveup');
    assert.match(r.error, /no package or backup of 3\.1\.0 to return to/);
    assert.equal(readJ(U(w, 'result.json')).outcome, 'failed');
    assert.equal(has(U(w, 'applying.json')), false);
    assert.deepEqual(s.ops.calls, []);
  } finally {
    cleanup(w);
  }
});

test('portable rollback: the previous executable comes back and is started', async () => {
  const w = await installed({ kind: 'portable' });
  try {
    assert.equal(fs.readFileSync(w.install.target, 'utf8'), 'portable package 3.2.0');
    const s = applier(w, { version: '3.2.0' });
    await s.a.atStart();
    assert.equal(has(`${w.install.target}.lf-old`), false, 'the old executable is removed once it has quit');
    await s.a.failed('x');
    await s.a.atStart();
    assert.deepEqual(await s.a.failed('y'), { action: 'exit' });
    assert.equal(fs.readFileSync(w.install.target, 'utf8'), 'portable 3.1.0');
    assert.deepEqual(s.ops.calls, [{ op: 'releaseLock' }, { op: 'spawn', file: w.install.target, args: [UPDATED_ARG], verbatim: false }]);
    const old = applier(w, { version: '3.1.0' });
    await old.a.atStart();
    assert.equal(readJ(U(w, 'result.json')).outcome, 'rolled_back');
    assert.equal(has(`${w.install.target}.lf-old`), false);
  } finally {
    cleanup(w);
  }
});

test('macOS rollback: the backed-up app is swapped back', async () => {
  const w = await installed({ kind: 'zip' });
  try {
    const s = applier(w, { version: '3.2.0' });
    await s.a.atStart();
    await s.a.failed('x');
    await s.a.atStart();
    assert.deepEqual(await s.a.failed('y'), { action: 'exit' });
    assert.equal(appMarker(w.install.target), 'mac 3.1.0');
    assert.deepEqual(fs.readdirSync(path.dirname(w.install.target)), ['LectureFlow.app'], 'the failed app is removed');
    assert.deepEqual(s.ops.calls.slice(-2), [{ op: 'releaseLock' }, { op: 'spawn', file: '/usr/bin/open', args: ['-n', w.install.target, '--args', UPDATED_ARG], verbatim: false }]);
  } finally {
    cleanup(w);
  }
});

// ---- what the old version finds ----

test('the installer did not install the new version: reported as failed', async () => {
  const w = await installed();
  try {
    const { a } = applier(w, { version: '3.1.0', now: () => new Date(NOW.getTime() + HELPER_WAIT_MS + 1000) });
    assert.equal((await a.atStart()).action, 'continue');
    const r = readJ(U(w, 'result.json'));
    assert.equal(r.outcome, 'failed');
    assert.match(r.error, /the installer did not install 3\.2\.0; 3\.1\.0 is still installed/);
    assert.equal(has(U(w, 'applying.json')), false);
    assert.equal(has(U(w, 'backup', '3.1.0', 'db')), false);
  } finally {
    cleanup(w);
  }
});

test('the installer is still running: this start ends (the installer starts the app)', async () => {
  const w = await installed();
  try {
    const { a } = applier(w, { version: '3.1.0', ops: fakeOps({ alive: (p) => p === 4001 }) });
    assert.deepEqual(await a.atStart(), { action: 'exit', why: 'busy' });
    assert.ok(has(U(w, 'applying.json')), 'nothing decided yet');
    // Long after, the same pid counts as gone.
    const late = applier(w, { version: '3.1.0', ops: fakeOps({ alive: () => true }), now: () => new Date(NOW.getTime() + HELPER_WAIT_MS + 1) });
    assert.equal((await late.a.atStart()).action, 'continue');
  } finally {
    cleanup(w);
  }
});

test('applying.json that cannot be read, or names other versions, is dropped', async () => {
  for (const body of ['{not json', JSON.stringify({ schema: 1, version: 'x' }), JSON.stringify({ schema: 1, version: '3.4.0', from_version: '3.3.0', phase: 'installing' })]) {
    const w = world();
    try {
      fs.writeFileSync(U(w, 'applying.json'), body);
      const { a, ops } = applier(w);
      assert.equal((await a.atStart()).action, 'continue');
      assert.equal(has(U(w, 'applying.json')), false);
      assert.deepEqual(ops.calls, []);
    } finally {
      cleanup(w);
    }
  }
});

test('leftovers of earlier installations are removed; other files are not', async () => {
  const p = world({ kind: 'portable' });
  const m = world({ kind: 'zip' });
  const i = world();
  try {
    const dirP = path.dirname(p.install.target);
    fs.writeFileSync(`${p.install.target}.lf-old`, 'x');
    fs.writeFileSync(`${p.install.target}.lf-old-20261103T080000Z`, 'x');
    fs.writeFileSync(path.join(dirP, 'notes.txt'), 'mine');
    await applier(p).a.atStart();
    assert.deepEqual(fs.readdirSync(dirP).sort(), [path.basename(p.install.target), 'notes.txt'].sort());

    const dirM = path.dirname(m.install.target);
    fs.mkdirSync(path.join(dirM, '.LectureFlow.app.lf-old-20261103T080000Z'));
    fs.mkdirSync(path.join(dirM, '.LectureFlow.app.lf-new-3.2.0'));
    fs.mkdirSync(path.join(dirM, 'Other.app'));
    await applier(m).a.atStart();
    assert.deepEqual(fs.readdirSync(dirM).sort(), ['LectureFlow.app', 'Other.app']);

    const dirI = path.dirname(i.install.target);
    fs.mkdirSync(path.join(dirI, 'LectureFlow.lf-failed-20261103T080000Z'));
    fs.mkdirSync(path.join(dirI, 'LectureFlowData'));
    await applier(i).a.atStart();
    assert.deepEqual(fs.readdirSync(dirI).sort(), ['LectureFlow', 'LectureFlowData']);
  } finally {
    cleanup(p);
    cleanup(m);
    cleanup(i);
  }
});

test('only the backup previous.json names is kept', async () => {
  const w = await installed({ withFile: false });
  try {
    fs.mkdirSync(U(w, 'backup', '3.0.0', 'app'), { recursive: true });
    const s = applier(w, { version: '3.2.0' });
    await s.a.atStart();
    await s.a.healthy();
    assert.deepEqual(fs.readdirSync(U(w, 'backup')), ['3.1.0']);
  } finally {
    cleanup(w);
  }
});

test('JSON files are written atomically (no temporary file left behind)', async () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'lf-json-'));
  try {
    const f = path.join(dir, 'result.json');
    await F.writeJsonAtomic(f, { a: 1 });
    await F.writeJsonAtomic(f, { a: 2 });
    assert.deepEqual(readJ(f), { a: 2 });
    assert.deepEqual(fs.readdirSync(dir), ['result.json']);
    assert.deepEqual(await F.readJson(path.join(dir, 'missing.json')), { exists: false });
    fs.writeFileSync(path.join(dir, 'bad.json'), '[1]');
    assert.match((await F.readJson(path.join(dir, 'bad.json'))).error, /not an object/);
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

test('the shell installs only while the service is stopped (source order)', () => {
  const src = fs.readFileSync(path.join(__dirname, '..', '..', 'src', 'main', 'shell.js'), 'utf8');
  const start = src.slice(src.indexOf('async startServer('), src.indexOf('// ---- in-app update'));
  assert.ok(start.indexOf('previous.stop(') < start.indexOf('_selfUpdateAtStart('), 'the previous service is stopped first');
  assert.ok(start.indexOf('_selfUpdateAtStart(') < start.indexOf('new ServerProcess('), 'updates are applied before the service starts');
  const failed = src.slice(src.indexOf('async _probationFailed('));
  assert.ok(failed.indexOf('server.stop(') < failed.indexOf('this.applier.failed('), 'the service is stopped before a rollback');
});

// ---- security review sec-update (M2, M3) ----

test('a planted applying.json (no record in the profile) is ignored: nothing is rolled back or run', async () => {
  for (const background of [false, true]) {
    const w = world();
    try {
      // what another account could leave in a data folder it can write:
      // a "failed" installation, previous.json and a backup with a program
      writePrevious(w, { backup: 'backup/3.0.0', version: '3.0.0' });
      fs.mkdirSync(U(w, 'backup', '3.0.0', 'app'), { recursive: true });
      fs.writeFileSync(U(w, 'backup', '3.0.0', 'app', 'LectureFlow.exe'), 'evil');
      fs.writeFileSync(U(w, 'backup', '3.0.0', 'backup.json'), JSON.stringify({ schema: 1, version: '3.0.0', kind: 'installer', item: '.' }));
      fs.writeFileSync(U(w, 'applying.json'), JSON.stringify({
        schema: 1, version: '3.1.0', from_version: '3.0.0', platform: 'windows-x64', kind: 'installer', phase: 'installing', tries: 5,
        target: w.install.target, exe: 'LectureFlow.exe',
      }));
      const { a, ops, logs } = applier(w);
      assert.deepEqual(await a.atStart({ background }), { action: 'continue' });
      assert.deepEqual(ops.calls, [], 'nothing started');
      assert.deepEqual(ops.verifies, []);
      assert.equal(has(U(w, 'applying.json')), false);
      assert.equal(has(U(w, 'result.json')), false);
      assert.ok(logs.some((l) => /ignoring applying\.json.*did not start an installation/.test(l)), logs.join('\n'));
    } finally {
      cleanup(w);
    }
  }
});

test('applying.json of another place (target) is ignored, even with a record of the same versions', async () => {
  const w = await installed();
  try {
    const ap = readJ(U(w, 'applying.json'));
    fs.writeFileSync(U(w, 'applying.json'), JSON.stringify({ ...ap, target: path.join(w.root, 'Elsewhere') }));
    const s = applier(w, { version: '3.2.0' });
    assert.deepEqual(await s.a.atStart(), { action: 'continue' });
    assert.equal(has(U(w, 'applying.json')), false);
    assert.deepEqual(s.ops.calls, []);
  } finally {
    cleanup(w);
  }
});

/** failTwice(w, ops): the new version 3.2.0 fails its two starts → { s, r: the second failed()'s result }. */
async function failTwice(w, ops) {
  const s = applier(w, { version: '3.2.0', ops });
  assert.equal((await s.a.atStart()).action, 'probation');
  await s.a.failed('x');
  await s.a.atStart();
  return { s, r: await s.a.failed('the service did not answer within 30 seconds') };
}

for (const [name, run, expect] of [
  ['other accounts can change the data folder (exit 3)', { code: 3, stdout: 'untrusted_dir: other accounts of this computer can change D:\\LF (Authenticated Users)\n' },
    /other accounts of this computer can change the data folder/],
  ['the check cannot run', { error: 'spawn lectureflow.exe ENOENT' }, /the previous version could not be checked: the update check could not run/],
]) {
  test(`no rollback when ${name}: reported, nothing run, the database stays`, async () => {
    const w = await installed({ withFile: true });
    try {
      const verify = (dir, v, o) => (o.previous ? { code: null, stdout: '', stderr: '', timedOut: false, error: null, ...run } : goVerifies(dir, v, o));
      const { s, r } = await failTwice(w, fakeOps({ verify }));
      assert.equal(r.action, 'giveup');
      assert.match(r.error, expect);
      assert.deepEqual(s.ops.calls, []);
      assert.deepEqual(s.ops.verifies.map((x) => x.previous), [true]);
      const res = readJ(U(w, 'result.json'));
      assert.equal(res.outcome, 'failed');
      assert.match(res.error, expect);
      assert.equal(fs.readFileSync(path.join(w.dataDir, 'lectureflow.db'), 'utf8'), 'db migrated by 3.2.0', 'the database is not put back');
      assert.equal(has(U(w, 'applying.json')), false);
      assert.equal(trust(w).install, undefined, 'the record goes with applying.json');
      assert.deepEqual(copies(w), []);
    } finally {
      cleanup(w);
    }
  });
}

test('rollback: a previous package the service program refuses is not run; the recorded backup is used', async () => {
  const w = await installed(); // a backup of 3.1.0 was made
  try {
    // previous.json also names a package (it does not verify)
    const prev = readJ(U(w, 'previous.json'));
    fs.writeFileSync(U(w, 'previous.json'), JSON.stringify({ ...prev, ...pkg(w, '3.1.0', 'LectureFlow-Setup-3.1.0-x64.exe', 'tampered installer') }));
    const verify = (dir, v, o) => (o.previous
      ? { code: 1, stdout: 'invalid selfupdate.signature_invalid: update.json.minisig does not verify\n', stderr: '', timedOut: false, error: null }
      : goVerifies(dir, v, o));
    const { s, r } = await failTwice(w, fakeOps({ verify }));
    assert.deepEqual(r, { action: 'exit' });
    const helper = path.join(U(w, 'backup', '3.1.0', 'app'), 'LectureFlow.exe');
    assert.deepEqual(s.ops.calls, [{ op: 'releaseLock' }, { op: 'spawn', file: helper, args: [RESTORE_ARG], verbatim: false }]);
    assert.equal(readJ(U(w, 'applying.json')).restore, 'backup');
    assert.ok(s.logs.some((l) => /the package of 3\.1\.0 cannot be used: .*signature_invalid/.test(l)), s.logs.join('\n'));
  } finally {
    cleanup(w);
  }
});

for (const [name, change] of [
  ['a file of the backup changed', (w) => fs.writeFileSync(U(w, 'backup', '3.1.0', 'app', 'resources', 'app.asar'), 'evil asar')],
  ['a file added to the backup', (w) => fs.writeFileSync(U(w, 'backup', '3.1.0', 'app', 'evil.dll'), 'evil')],
  ['no record of the backup in the profile', (w) => {
    const t = trust(w);
    delete t.backups['3.1.0'];
    fs.writeFileSync(path.join(w.profile, F.TRUST_FILE), JSON.stringify(t));
  }],
]) {
  test(`rollback: a backup is not used when ${name}`, async () => {
    const w = await installed();
    try {
      change(w);
      const { s, r } = await failTwice(w);
      assert.equal(r.action, 'giveup');
      assert.match(r.error, /no package or backup of 3\.1\.0 to return to/);
      assert.deepEqual(s.ops.calls, []);
    } finally {
      cleanup(w);
    }
  });
}

test('rollback in a build without a public key: the previous package is not run; the result says why', async () => {
  const w = await installed({ withFile: true });
  try {
    const verify = (dir, v, o) => (o.previous ? { code: 2, stdout: 'not_configured: this build has no update public key\n', stderr: '', timedOut: false, error: null } : goVerifies(dir, v, o));
    const { s, r } = await failTwice(w, fakeOps({ verify }));
    assert.equal(r.action, 'giveup');
    assert.match(r.error, /no package or backup of 3\.1\.0 to return to \(its package: this build has no update public key/);
    assert.deepEqual(s.ops.calls, []);
    assert.match(readJ(U(w, 'result.json')).error, /no update public key/);
  } finally {
    cleanup(w);
  }
});

test('the restore program refuses a rollback the profile does not record (another target, no record)', async () => {
  const w = await installed();
  try {
    await failTwice(w);
    const helper = path.join(U(w, 'backup', '3.1.0', 'app'), 'LectureFlow.exe');
    const ap = readJ(U(w, 'applying.json'));
    // a target changed in applying.json: refused
    fs.writeFileSync(U(w, 'applying.json'), JSON.stringify({ ...ap, target: path.join(w.root, 'Windows') }));
    const h = applier(w, { version: '3.1.0' });
    assert.equal(await h.a.restoreFromBackup({ execPath: helper }), false);
    assert.deepEqual(h.ops.calls, []);
    // the record gone: refused
    fs.writeFileSync(U(w, 'applying.json'), JSON.stringify(ap));
    const t = trust(w);
    delete t.install;
    fs.writeFileSync(path.join(w.profile, F.TRUST_FILE), JSON.stringify(t));
    const h2 = applier(w, { version: '3.1.0' });
    assert.equal(await h2.a.restoreFromBackup({ execPath: helper }), false);
    assert.equal(fs.readFileSync(path.join(w.install.target, 'LectureFlow.exe'), 'utf8'), 'electron 3.2.0');
  } finally {
    cleanup(w);
  }
});

test('private copies: removed when not needed; old ones left by an installer go at a later start', async () => {
  const w = world();
  try {
    // a backup that cannot be made: the copy goes with the refusal
    writePending(w);
    writePrevious(w);
    fs.writeFileSync(U(w, 'backup'), 'not a directory');
    assert.equal((await applier(w).a.atStart()).action, 'continue');
    assert.deepEqual(copies(w), []);
    // copies of earlier starts: old ones are removed, a fresh one and other names stay
    const old = path.join(w.tmp, 'lectureflow-update-111');
    const fresh = path.join(w.tmp, 'lectureflow-update-222');
    const other = path.join(w.tmp, 'lectureflow-other-333');
    for (const d of [old, fresh, other]) fs.mkdirSync(d);
    const t0 = fs.statSync(fresh).mtimeMs;
    fs.utimesSync(old, new Date(t0 - HELPER_WAIT_MS - 60000), new Date(t0 - HELPER_WAIT_MS - 60000));
    await applier(w, { now: () => new Date(t0 + 1000) }).a.atStart();
    assert.deepEqual(fs.readdirSync(w.tmp).sort(), ['lectureflow-other-333', 'lectureflow-update-222']);
  } finally {
    cleanup(w);
  }
});

test('tree hashes: the same tree gives the same hash anywhere; any change gives another', async () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'lf-tree-'));
  try {
    const a = path.join(root, 'a');
    fs.mkdirSync(path.join(a, 'resources'), { recursive: true });
    fs.writeFileSync(path.join(a, 'LectureFlow.exe'), 'exe');
    fs.writeFileSync(path.join(a, 'resources', 'app.asar'), 'asar');
    fs.cpSync(a, path.join(root, 'b'), { recursive: true });
    const ha = await F.treeHash(a);
    assert.deepEqual(await F.treeHash(path.join(root, 'b')), ha);
    assert.equal(ha.files, 2);
    assert.equal(ha.bytes, 7);
    fs.writeFileSync(path.join(root, 'b', 'resources', 'app.asar'), 'asaR');
    assert.notEqual((await F.treeHash(path.join(root, 'b'))).sha256, ha.sha256);
    fs.mkdirSync(path.join(a, 'empty'));
    assert.notEqual((await F.treeHash(a)).sha256, ha.sha256, 'an empty folder counts');
    assert.equal((await F.treeHash(path.join(a, 'LectureFlow.exe'))).files, 1, 'a single file');
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
});
