// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The Firefox copy of npm run test:firefox (test/firefox/provision.mjs):
// the pin, where the copy lives, how a download is checked, and that the
// tests never fall back to the Firefox installed on the machine. No network
// and no Firefox here; the copy itself is exercised by npm run test:firefox.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import crypto from 'node:crypto';
import {
  FIREFOX, POLICIES, UPDATE_PREFS, packageFor, appBinary, distributionDir, updaterFiles,
  mainCheckout, defaultCacheRoot, copyDir, checkCopy, download, firefoxForTests,
} from '../firefox/provision.mjs';

function tmp(t) {
  const d = fs.mkdtempSync(path.join(os.tmpdir(), 'lf-ext-fxprov-'));
  t.after(() => fs.rmSync(d, { recursive: true, force: true }));
  return d;
}
const sha512 = (buf) => crypto.createHash('sha512').update(buf).digest('hex');
const fakeFetch = (body, status = 200) => async () => new Response(status === 200 ? body : 'no', { status, headers: { 'content-length': String(body.length) } });

test('the pin: an ESR from archive.mozilla.org, one package per supported machine, each with a SHA-512', () => {
  assert.match(FIREFOX.version, /^\d+\.\d+(\.\d+)?esr$/);
  assert.equal(FIREFOX.base, `https://archive.mozilla.org/pub/firefox/releases/${FIREFOX.version}/`);
  for (const [platform, arch, key] of [['win32', 'x64', 'win64'], ['win32', 'arm64', 'win64-aarch64'], ['darwin', 'arm64', 'mac'], ['darwin', 'x64', 'mac'], ['linux', 'x64', 'linux-x86_64'], ['linux', 'arm64', 'linux-aarch64']]) {
    const p = packageFor(platform, arch);
    assert.equal(p.key, key);
    assert.match(p.sha512, /^[0-9a-f]{128}$/);
    assert.ok(p.file.startsWith(`${key}/en-US/`) && p.file.includes(FIREFOX.version), p.file);
    assert.ok(p.url.startsWith(FIREFOX.base) && !p.url.includes(' '), p.url);
    assert.equal(decodeURIComponent(p.url.slice(FIREFOX.base.length)), p.file);
  }
  assert.equal(packageFor('win32', 'x64').url, `${FIREFOX.base}win64/en-US/Firefox%20Setup%20${FIREFOX.version}.exe`);
  assert.equal(packageFor('freebsd', 'x64'), null);
  assert.equal(packageFor('linux', 'ia32'), null);
});

test('the copy: binary, policies and updater paths per platform; the policy and prefs that switch updates off', () => {
  const app = path.join('c', 'app');
  assert.equal(appBinary(app, 'win32'), path.join(app, 'firefox.exe'));
  assert.equal(appBinary(app, 'darwin'), path.join(app, 'Firefox.app', 'Contents', 'MacOS', 'firefox'));
  assert.equal(appBinary(app, 'linux'), path.join(app, 'firefox'));
  assert.equal(distributionDir(app, 'win32'), path.join(app, 'distribution'));
  assert.equal(distributionDir(app, 'darwin'), path.join(app, 'Firefox.app', 'Contents', 'Resources', 'distribution'));
  assert.equal(distributionDir(app, 'linux'), path.join(app, 'distribution'));
  assert.ok(updaterFiles(app, 'win32').includes(path.join(app, 'updater.exe')));
  assert.ok(updaterFiles(app, 'win32').includes(path.join(app, 'maintenanceservice.exe')));
  assert.ok(updaterFiles(app, 'linux').includes(path.join(app, 'updater')));
  assert.equal(POLICIES.policies.DisableAppUpdate, true);
  assert.equal(UPDATE_PREFS['app.update.disabledForTesting'], true);
  assert.equal(UPDATE_PREFS['app.update.auto'], false);
  assert.ok(!('app.update.enabled' in UPDATE_PREFS), 'removed in Firefox 63');
});

test('the cache: <checkout>/tools/firefox, a worktree uses its main checkout, LF_FIREFOX_CACHE wins', (t) => {
  const d = tmp(t);
  const main = path.join(d, 'repo');
  fs.mkdirSync(path.join(main, '.git', 'worktrees', 'agent-1'), { recursive: true });
  assert.equal(mainCheckout(main), main);
  const wt = path.join(main, '.claude', 'worktrees', 'agent-1');
  fs.mkdirSync(wt, { recursive: true });
  fs.writeFileSync(path.join(wt, '.git'), `gitdir: ${path.join(main, '.git', 'worktrees', 'agent-1').replace(/\\/g, '/')}\n`);
  assert.equal(mainCheckout(wt), main);
  assert.equal(defaultCacheRoot({ env: {}, top: wt }), path.join(main, 'tools', 'firefox'));
  // a .git file that is not a worktree's (a submodule, say): the checkout itself
  const sub = path.join(d, 'sub');
  fs.mkdirSync(sub);
  fs.writeFileSync(path.join(sub, '.git'), 'gitdir: ../repo/.git/modules/sub\n');
  assert.equal(mainCheckout(sub), sub);
  assert.equal(mainCheckout(path.join(d, 'nowhere')), path.join(d, 'nowhere'));
  assert.equal(defaultCacheRoot({ env: { LF_FIREFOX_CACHE: path.join(d, 'c') }, top: wt }), path.join(d, 'c'));
  const pkg = packageFor('win32', 'x64');
  assert.equal(copyDir(path.join(d, 'c'), pkg), path.join(d, 'c', `${FIREFOX.version}-win64`));
});

test('a copy counts only when complete and of the pinned version', (t) => {
  const pkg = packageFor('linux', 'x64');
  const dir = path.join(tmp(t), 'copy');
  assert.equal(checkCopy(dir, pkg, 'linux').ok, false);
  fs.mkdirSync(path.join(dir, 'app'), { recursive: true });
  const marker = (v, s) => fs.writeFileSync(path.join(dir, 'lectureflow-firefox.json'), JSON.stringify({ version: v, sha512: s }));
  marker(FIREFOX.version, pkg.sha512);
  assert.deepEqual(checkCopy(dir, pkg, 'linux'), { ok: false, reason: 'incomplete copy' });
  fs.writeFileSync(path.join(dir, 'app', 'firefox'), '');
  assert.equal(checkCopy(dir, pkg, 'linux').ok, true);
  marker('140.0esr', pkg.sha512);
  assert.equal(checkCopy(dir, pkg, 'linux').ok, false);
  marker(FIREFOX.version, '0'.repeat(128));
  assert.equal(checkCopy(dir, pkg, 'linux').ok, false);
});

test('a download is kept only when its SHA-512 matches; nothing is left otherwise', async (t) => {
  const d = tmp(t);
  const body = Buffer.from('not really firefox');
  const dest = path.join(d, 'pkg.bin');
  await assert.rejects(download('https://example.invalid/x', dest, '0'.repeat(128), { fetchImpl: fakeFetch(body) }), /SHA-512 mismatch/);
  assert.deepEqual(fs.readdirSync(d), []);
  await assert.rejects(download('https://example.invalid/x', dest, sha512(body), { fetchImpl: fakeFetch(body, 404) }), /HTTP 404/);
  assert.deepEqual(fs.readdirSync(d), []);
  await download('https://example.invalid/x', dest, sha512(body), { fetchImpl: fakeFetch(body) });
  assert.deepEqual(fs.readdirSync(d), ['pkg.bin']);
  assert.equal(fs.readFileSync(dest, 'utf8'), 'not really firefox');
});

test('no copy and no download: the tests fail with instructions and never take the installed Firefox', async (t) => {
  const d = tmp(t);
  const offline = async () => {
    throw new TypeError('fetch failed');
  };
  const err = await firefoxForTests({ env: { LF_FIREFOX_CACHE: path.join(d, 'cache') }, top: d, platform: 'win32', arch: 'x64', fetchImpl: offline }).then(() => null, (e) => e);
  assert.ok(err, 'it throws');
  assert.match(err.message, /fetch failed/);
  assert.match(err.message, /npm run firefox:prepare/);
  assert.match(err.message, /LF_FIREFOX=/);
  assert.ok(err.message.includes(packageFor('win32', 'x64').sha512), 'the offline route names the hash');
  assert.ok(!/Program Files|\/Applications\//.test(err.message), err.message);
  // nothing half-done is left in the cache
  assert.deepEqual(fs.readdirSync(path.join(d, 'cache')).filter((n) => n !== 'downloads'), []);
  assert.deepEqual(fs.readdirSync(path.join(d, 'cache', 'downloads')), []);
  // a platform without a pinned package fails the same way
  await assert.rejects(firefoxForTests({ env: { LF_FIREFOX_CACHE: path.join(d, 'cache') }, top: d, platform: 'freebsd', arch: 'x64', fetchImpl: offline }), /no Firefox package is pinned[\s\S]*LF_FIREFOX=/);
});

test('LF_FIREFOX is used as given, without touching the cache; a path that does not exist is an error', async (t) => {
  const d = tmp(t);
  const bin = path.join(d, 'firefox-bin');
  fs.writeFileSync(bin, '');
  const never = async () => assert.fail('no download with LF_FIREFOX');
  const env = { LF_FIREFOX: bin, LF_FIREFOX_CACHE: path.join(d, 'cache') };
  assert.deepEqual(await firefoxForTests({ env, top: d, fetchImpl: never }), { binary: bin, source: 'LF_FIREFOX', version: null, ownDirs: [], cacheRoot: null });
  assert.equal(fs.existsSync(path.join(d, 'cache')), false);
  assert.equal((await firefoxForTests({ env: { LF_FIREFOX: 'firefox' }, top: d, fetchImpl: never })).binary, 'firefox');
  await assert.rejects(firefoxForTests({ env: { LF_FIREFOX: path.join(d, 'missing', 'firefox') }, top: d, fetchImpl: never }), /no such file/);
});
