// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The service program's last check of a pending update before it is
// installed (lib/verifyupdate.js; contract: internal/selfupdate/README.md
// "What Electron does at start", cmd/lectureflow/selfupdate_verify.go).
'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');

const {
  VERIFY_TIMEOUT_MS, verifyArgs, stripQueries, runVerify, interpretVerify, copyPath,
} = require('../../src/main/lib/verifyupdate');

const SHA = 'ab'.repeat(32);
const PENDING = { version: '3.2.0', kind: 'installer', file: '3.2.0/LectureFlow-Setup-3.2.0-x64.exe', sha256: SHA };
const ok = (over = {}) => {
  const p = { ...PENDING, ...over };
  return { code: 0, stdout: `ok version=${p.version} kind=${p.kind} file=${p.file} sha256=${p.sha256}\n`, stderr: '', timedOut: false, error: null };
};

test('arguments: the updates folder and the running version, as the README says', () => {
  assert.deepEqual(verifyArgs('C:\\Users\\A B\\LectureFlow\\updates', '3.1.0'), ['-selfupdate-verify', 'C:\\Users\\A B\\LectureFlow\\updates', '-app-version', '3.1.0']);
  assert.deepEqual(verifyArgs('/data/updates', ''), ['-selfupdate-verify', '/data/updates']);
  // sec-update: the previous version's package, and the private copy
  assert.deepEqual(verifyArgs('/data/updates', '3.2.0', { previous: true, copyTo: '/tmp/x y' }),
    ['-selfupdate-verify', '/data/updates', '-app-version', '3.2.0', '-selfupdate-previous', '-selfupdate-copy', '/tmp/x y']);
  assert.equal(VERIFY_TIMEOUT_MS, 120000, 'the check copies the package too');
});

test('query strings and fragments never reach the log', () => {
  assert.equal(stripQueries('see https://h.example/a/b.json?sig=SECRET&x=1 and http://x.test/p#frag end'), 'see https://h.example/a/b.json and http://x.test/p end');
  assert.equal(stripQueries('no url? here # fine'), 'no url? here # fine');
  assert.equal(stripQueries(null), '');
});

test('exit 0 installs only the package pending.json names', () => {
  assert.deepEqual(interpretVerify(ok(), PENDING), { install: true, line: ok().stdout.trim(), copy: null, verdict: 'ok' });
  assert.equal(interpretVerify({ ...ok(), stdout: ok().stdout.replace('\n', '\r\n') }, PENDING).install, true, 'CRLF');
  const spaced = { ...PENDING, file: '3.2.0/LectureFlow Setup 3.2.0.exe' };
  assert.equal(interpretVerify(ok(spaced), spaced).install, true, 'a file name with spaces');
  for (const over of [{ version: '3.2.1' }, { kind: 'portable' }, { file: '3.2.0/other.exe' }, { sha256: 'cd'.repeat(32) }]) {
    const v = interpretVerify(ok(over), PENDING);
    assert.equal(v.install, undefined, JSON.stringify(over));
    assert.match(v.drop, /vouched for .* not the pending 3\.2\.0/, JSON.stringify(over));
  }
  for (const stdout of ['', 'ok\n', `ok version=3.2.0 kind=installer file=${PENDING.file} sha256=${SHA.toUpperCase()}\n`, `x\n${ok().stdout}`]) {
    assert.match(interpretVerify({ code: 0, stdout }, PENDING).keep, /no usable answer \(exit code 0\)/, JSON.stringify(stdout));
  }
});

test('exit 1 "invalid": drop; anything else: no verdict', () => {
  assert.deepEqual(interpretVerify({ code: 1, stdout: 'invalid selfupdate.checksum: the package has 5 bytes, SHA-256 00\n' }, PENDING), {
    drop: 'the update did not pass the check with the built-in key (selfupdate.checksum): the package has 5 bytes, SHA-256 00',
    verdict: 'invalid',
    code: 'selfupdate.checksum',
  });
  assert.match(interpretVerify({ code: 1, stdout: 'invalid : something odd\n' }, PENDING).drop, /\(invalid\): something odd/, 'an error without a code');
  assert.equal(interpretVerify({ code: 1, stdout: 'invalid selfupdate.manifest_invalid: bad url https://e.example/x?t=1\n' }, PENDING).drop.includes('?t='), false);
  assert.deepEqual(interpretVerify({ code: 2, stdout: 'not_configured: this build has no update public key\n' }, PENDING), {
    keep: 'this build has no update public key, so the update cannot be verified here',
    verdict: 'not_configured',
  });
  assert.match(interpretVerify({ code: 2, stdout: 'invalid x: y\n' }, PENDING).keep, /no usable answer \(exit code 2\)/);
  assert.match(interpretVerify({ code: 1, stdout: '', stderr: 'boot\nERROR: flag provided but not defined: -selfupdate-verify\n' }, PENDING).keep,
    /no usable answer \(exit code 1\): ERROR: flag provided but not defined: -selfupdate-verify$/);
  assert.match(interpretVerify({ code: 0, timedOut: true }, PENDING).keep, /within 120 seconds/);
  assert.match(interpretVerify({ timedOut: true }, PENDING, { timeoutMs: 5000 }).keep, /within 5 seconds/);
  assert.match(interpretVerify({ code: null, error: 'spawn x ENOENT' }, PENDING).keep, /could not run: spawn x ENOENT/);
  assert.match(interpretVerify(null, PENDING).keep, /no usable answer/);
});

test('exit 3: other accounts can change the data folder: no verdict, nothing installed or rolled back', () => {
  const v = interpretVerify({ code: 3, stdout: 'untrusted_dir: selfupdate.untrusted_dir: other accounts of this computer can change D:\\LF (Authenticated Users)\n' }, PENDING);
  assert.equal(v.verdict, 'untrusted');
  assert.equal(v.install, undefined);
  assert.equal(v.drop, undefined, 'pending.json is kept: it is installed once the permissions are tightened');
  assert.match(v.keep, /other accounts of this computer can change the data folder.*Diagnostics.*Authenticated Users/);
  assert.equal(interpretVerify({ code: 3, stdout: 'invalid x: y\n' }, PENDING).verdict, 'none', 'exit 3 without the untrusted_dir line');
});

// sec-update M3: with copyTo the program copies the package into a new
// private folder there and names it; the shell runs only that copy.
test('the private copy: only one where it was asked for is accepted', () => {
  // A short fixed folder (interpretVerify only compares paths, nothing is
  // created): below a long os.tmpdir() the ok line passes 300 characters and
  // the expected `line` would be the clipped one (pack-dry 2026-10-27).
  const tmp = path.join(path.parse(os.tmpdir()).root, 'lf tmp');
  const name = path.posix.basename(PENDING.file);
  const good = path.join(tmp, 'lectureflow-update-123456', name);
  const withCopy = (copy, over = {}) => ({ ...ok(over), stdout: `${ok(over).stdout.trim()} copy=${copy}\n` });
  assert.deepEqual(interpretVerify(withCopy(good), PENDING, { copyTo: tmp }), {
    install: true, line: withCopy(good).stdout.trim(), copy: path.resolve(good), verdict: 'ok',
  });
  for (const bad of [
    path.join(os.tmpdir(), 'other', 'lectureflow-update-1', name), // another folder
    path.join(tmp, 'lectureflow-update-1', 'evil.exe'), // another name
    path.join(tmp, 'x-1', name), // not a copy folder
    path.join(tmp, name), // not in its own folder
    `lectureflow-update-1/${name}`, // relative
  ]) {
    const v = interpretVerify(withCopy(bad), PENDING, { copyTo: tmp });
    assert.equal(v.install, undefined, bad);
    assert.match(v.keep, /did not copy the package where it was asked to/, bad);
  }
  assert.match(interpretVerify(ok(), PENDING, { copyTo: tmp }).keep, /did not copy the package/, 'no copy at all');
  // vouching for another package: dropped, and the copy is named for removal
  const other = interpretVerify(withCopy(path.join(tmp, 'lectureflow-update-9', name), { sha256: 'cd'.repeat(32) }), PENDING, { copyTo: tmp });
  assert.match(other.drop, /not the pending 3\.2\.0/);
  assert.equal(other.copy, path.resolve(path.join(tmp, 'lectureflow-update-9', name)));
  assert.match(interpretVerify(ok({ version: '3.1.0' }), PENDING, { what: 'previous' }).drop, /not the previous 3\.2\.0/);
  assert.equal(copyPath(good, tmp, PENDING.file), path.resolve(good));
  assert.equal(copyPath(good, '', PENDING.file), null);
});

// ---- running the program ----

function scripts() {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'lf-verify-'));
  const write = (name, body) => {
    const f = path.join(dir, name);
    fs.writeFileSync(f, body);
    return f;
  };
  return {
    dir,
    echo: write('echo.js', `process.stdout.write(JSON.stringify({ args: process.argv.slice(2), env: process.env.LF_VERIFY_TEST || '' }) + '\\n');
process.stderr.write('note on stderr\\n');
process.exitCode = Number(process.env.LF_VERIFY_EXIT || 0);
`),
    hang: write('hang.js', 'setInterval(() => {}, 1000);\n'),
  };
}

test('runVerify runs the program with the arguments (spaces kept), the environment and its prefix', async () => {
  const s = scripts();
  try {
    const updates = path.join(s.dir, 'data dir', 'updates');
    const cmd = { command: process.execPath, prefixArgs: [s.echo], env: { ...process.env, LF_VERIFY_EXIT: '2' }, extraEnv: { LF_VERIFY_TEST: 'extra' } };
    const r = await runVerify(cmd, updates, '3.1.0');
    assert.equal(r.code, 2);
    assert.equal(r.timedOut, false);
    assert.equal(r.error, null);
    assert.deepEqual(JSON.parse(r.stdout), { args: ['-selfupdate-verify', updates, '-app-version', '3.1.0'], env: 'extra' });
    assert.equal(r.stderr.trim(), 'note on stderr');
    const tmp = path.join(s.dir, 'temp dir');
    const p = await runVerify(cmd, updates, '3.2.0', { previous: true, copyTo: tmp });
    assert.deepEqual(JSON.parse(p.stdout).args, ['-selfupdate-verify', updates, '-app-version', '3.2.0', '-selfupdate-previous', '-selfupdate-copy', tmp]);
  } finally {
    fs.rmSync(s.dir, { recursive: true, force: true });
  }
});

test('runVerify: a program that hangs is stopped after the timeout; a missing one is an error', async () => {
  const s = scripts();
  try {
    const t0 = Date.now();
    const r = await runVerify({ command: process.execPath, prefixArgs: [s.hang] }, s.dir, '3.1.0', { timeoutMs: 400 });
    assert.equal(r.timedOut, true);
    assert.equal(r.code, null);
    assert.ok(Date.now() - t0 < 10000, 'did not wait for the program');
    assert.match(interpretVerify(r, PENDING, { timeoutMs: 400 }).keep, /did not finish/);

    const missing = await runVerify({ command: path.join(s.dir, 'no-such-program.exe') }, s.dir, '3.1.0');
    assert.equal(missing.code, null);
    assert.match(missing.error, /ENOENT/);
    assert.match(interpretVerify(missing, PENDING).keep, /could not run/);

    const thrown = await runVerify({ command: 'x' }, s.dir, '3.1.0', { spawn: () => { throw new Error('spawn refused'); } });
    assert.equal(thrown.error, 'spawn refused');
  } finally {
    fs.rmSync(s.dir, { recursive: true, force: true });
  }
});
