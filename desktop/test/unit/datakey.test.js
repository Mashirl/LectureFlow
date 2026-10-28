// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The credentials' data key on macOS (cred-enc): kept with safeStorage in
// the profile, sent to the service on the stdin line after the token. A
// fake safeStorage stands in for the Keychain; the services are small
// scripts that record what they received.
'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');

const { loadOrCreateDataKey, dataKeyForService, UNAVAILABLE, KEY_FILE } = require('../../src/main/datakey');
const { ServerProcess } = require('../../src/main/server');

/**
 * fakeSafeStorage: "encrypts" by prefixing a tag and reversing (per
 * Keychain); down: every call fails (access refused, locked).
 */
function fakeSafeStorage({ available = true, tag = 'kc1', down = false } = {}) {
  const refuse = () => {
    throw new Error('Error while decrypting the ciphertext provided to safeStorage.decryptString.');
  };
  return {
    isEncryptionAvailable: () => available,
    encryptString: (s) => (down ? refuse() : Buffer.from(`${tag}:${[...s].reverse().join('')}`)),
    decryptString: (b) => {
      if (down) refuse();
      const s = b.toString();
      if (!s.startsWith(`${tag}:`)) throw new Error('Error while decrypting the ciphertext provided to safeStorage.decryptString.');
      return [...s.slice(tag.length + 1)].reverse().join('');
    },
  };
}

function logger() {
  const lines = [];
  const rec = (level) => (m) => lines.push(`${level} ${m}`);
  return { lines, log: { info: rec('info'), warn: rec('warn'), error: rec('error') } };
}

function tmp() {
  return fs.mkdtempSync(path.join(os.tmpdir(), 'lf-datakey-'));
}

test('the first start makes a key, the next ones read the same key; the file holds no plain key', () => {
  const dir = tmp();
  const { lines, log } = logger();
  const k1 = loadOrCreateDataKey({ safeStorage: fakeSafeStorage(), dir, log });
  assert.ok(Buffer.isBuffer(k1) && k1.length === 32);
  const file = path.join(dir, KEY_FILE);
  const raw = fs.readFileSync(file);
  assert.ok(!raw.toString().includes(k1.toString('hex')), 'the file holds the key in the clear');
  if (process.platform !== 'win32') assert.equal(fs.statSync(file).mode & 0o777, 0o600);
  const k2 = loadOrCreateDataKey({ safeStorage: fakeSafeStorage(), dir, log });
  assert.deepEqual(k2, k1);
  assert.deepEqual(fs.readdirSync(dir), [KEY_FILE], 'no temporary or extra files');
  assert.ok(lines.every((l) => !l.includes(k1.toString('hex'))), 'the key is in the log');
  fs.rmSync(dir, { recursive: true, force: true });
});

test('a key file another Mac or account made is kept aside, never overwritten; a new key is made', () => {
  const dir = tmp();
  const first = loadOrCreateDataKey({ safeStorage: fakeSafeStorage({ tag: 'other-mac' }), dir });
  const before = fs.readFileSync(path.join(dir, KEY_FILE));
  const { lines, log } = logger();
  const now = () => new Date(2026, 9, 27, 12, 0, 0);
  const key = loadOrCreateDataKey({ safeStorage: fakeSafeStorage({ tag: 'this-mac' }), dir, log, now });
  assert.ok(Buffer.isBuffer(key) && !key.equals(first));
  const kept = path.join(dir, `${KEY_FILE}.unreadable-20261027-120000`);
  assert.deepEqual(fs.readFileSync(kept), before, 'the old key file was not kept as it was');
  assert.ok(lines.some((l) => l.startsWith('warn') && l.includes('entered again')), lines.join('\n'));
  // the new key file opens here from now on
  assert.deepEqual(loadOrCreateDataKey({ safeStorage: fakeSafeStorage({ tag: 'this-mac' }), dir }), key);
  assert.equal(fs.readdirSync(dir).length, 2);
  fs.rmSync(dir, { recursive: true, force: true });
});

test('without safeStorage no key is sent and nothing is written', () => {
  const dir = tmp();
  assert.equal(loadOrCreateDataKey({ safeStorage: fakeSafeStorage({ available: false }), dir }), UNAVAILABLE);
  assert.equal(loadOrCreateDataKey({ safeStorage: fakeSafeStorage({ down: true }), dir }), UNAVAILABLE, 'refused when making a key');
  assert.equal(loadOrCreateDataKey({ safeStorage: null, dir }), null);
  assert.deepEqual(fs.readdirSync(dir), []);
  fs.rmSync(dir, { recursive: true, force: true });
});

test('a key file that cannot be read now is left alone', () => {
  const dir = tmp();
  fs.mkdirSync(path.join(dir, KEY_FILE)); // reading a folder fails (not ENOENT)
  assert.equal(loadOrCreateDataKey({ safeStorage: fakeSafeStorage(), dir }), UNAVAILABLE);
  assert.ok(fs.statSync(path.join(dir, KEY_FILE)).isDirectory());
  assert.equal(fs.readdirSync(dir).length, 1);
  fs.rmSync(dir, { recursive: true, force: true });
});

// sec3 L4: refused or locked is not "another Mac": nothing is replaced.
test('a Keychain that refuses or is locked leaves datakey.bin alone; it opens again later', () => {
  const dir = tmp();
  const key = loadOrCreateDataKey({ safeStorage: fakeSafeStorage(), dir });
  const before = fs.readFileSync(path.join(dir, KEY_FILE));
  const { lines, log } = logger();
  assert.equal(loadOrCreateDataKey({ safeStorage: fakeSafeStorage({ down: true }), dir, log }), UNAVAILABLE);
  assert.deepEqual(fs.readdirSync(dir), [KEY_FILE], 'nothing kept aside, nothing new');
  assert.deepEqual(fs.readFileSync(path.join(dir, KEY_FILE)), before);
  assert.ok(lines.some((l) => l.startsWith('warn') && l.includes('left as it is')), lines.join('\n'));
  assert.deepEqual(loadOrCreateDataKey({ safeStorage: fakeSafeStorage(), dir }), key, 'the same key once the Keychain works');
  fs.rmSync(dir, { recursive: true, force: true });
});

test('a datakey.bin that decrypts to something else is replaced (the Keychain works)', () => {
  const dir = tmp();
  const ss = fakeSafeStorage();
  fs.writeFileSync(path.join(dir, KEY_FILE), ss.encryptString('not a key'));
  const key = loadOrCreateDataKey({ safeStorage: ss, dir, now: () => new Date(2026, 9, 27, 13, 0, 0) });
  assert.ok(Buffer.isBuffer(key) && key.length === 32);
  assert.ok(fs.existsSync(path.join(dir, `${KEY_FILE}.unreadable-20261027-130000`)));
  fs.rmSync(dir, { recursive: true, force: true });
});

test('dataKeyForService: macOS only, from Electron\'s profile folder', () => {
  const dir = tmp();
  const electron = { app: { getPath: (n) => (n === 'userData' ? dir : null) }, safeStorage: fakeSafeStorage() };
  assert.equal(dataKeyForService({ platform: 'win32', electron }), null);
  assert.equal(dataKeyForService({ platform: 'linux', electron }), null);
  const k = dataKeyForService({ platform: 'darwin', electron });
  assert.ok(Buffer.isBuffer(k) && k.length === 32);
  assert.ok(fs.existsSync(path.join(dir, KEY_FILE)));
  // plain Node: require('electron') is not the API
  assert.equal(dataKeyForService({ platform: 'darwin', electron: '/path/to/electron' }), null);
  fs.rmSync(dir, { recursive: true, force: true });
});

// ---- ServerProcess hands the key to the service ----

// Records its arguments and the stdin lines it received, prints the ready
// line, answers /api/health, and echoes the key line to stderr (as a
// careless service might).
const SERVICE = String.raw`'use strict';
const fs = require('node:fs');
const http = require('node:http');
const args = process.argv.slice(2);
const port = Number(args[args.indexOf('-listen') + 1].split(':')[1]);
const want = args.includes('-datakey-stdin') ? 2 : 1;
let buf = '';
let started = false;
process.stdin.setEncoding('utf8');
process.stdin.on('data', (c) => {
  buf += c;
  const lines = buf.split('\n');
  if (started || lines.length <= want) return;
  started = true;
  const got = lines.slice(0, want).map((l) => l.trim());
  fs.writeFileSync(process.env.DK_STATE, JSON.stringify({ args, lines: got }));
  if (want === 2) process.stderr.write('echo ' + got[1] + '\n');
  const token = got[0];
  http.createServer((req, res) => {
    const ok = req.headers['x-lf-token'] === token;
    res.writeHead(ok ? 200 : 401, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify(ok ? { ok: true, version: '9.9.9', mode: 'desktop' } : {}));
  }).listen(port, '127.0.0.1', () => process.stdout.write('LECTUREFLOW_READY http://127.0.0.1:' + port + '\n'));
});
process.stdin.on('end', () => process.exit(0));
`;

async function runService(dataKey, { echo = false } = {}) {
  const dir = tmp();
  const script = path.join(dir, 'service.js');
  fs.writeFileSync(script, SERVICE);
  const state = path.join(dir, 'state.json');
  const { lines, log } = logger();
  const srv = new ServerProcess({
    command: process.execPath,
    prefixArgs: [script],
    argsFor: (p) => ['-desktop', '-listen', `127.0.0.1:${p}`, '-data-dir', dir],
    env: { ...process.env, DK_STATE: state },
    cwd: dir,
    pollIntervalMs: 40,
    log,
    ...(dataKey === undefined ? {} : { dataKey }),
  });
  try {
    await srv.start();
    // stderr is read asynchronously: give the echo line a moment
    for (let i = 0; echo && i < 50 && !srv.stderrTail().length; i++) await new Promise((r) => setTimeout(r, 20));
    return { got: JSON.parse(fs.readFileSync(state, 'utf8')), stderr: srv.stderrTail(), lines };
  } finally {
    await srv.stop({ timeoutMs: 2000 }).catch(() => {});
    fs.rmSync(dir, { recursive: true, force: true });
  }
}

test('with a key the service gets -datakey-stdin and the key as the line after the token; logs redact it', async () => {
  const key = Buffer.alloc(32, 0x5c);
  const hex = key.toString('hex');
  const { got, stderr, lines } = await runService(() => key, { echo: true });
  assert.ok(got.args.includes('-datakey-stdin'), got.args.join(' '));
  assert.equal(got.lines.length, 2);
  assert.match(got.lines[0], /^[0-9a-f]{64}$/, 'the token comes first');
  assert.equal(got.lines[1], hex);
  assert.ok(stderr.length > 0, 'the echo line arrived');
  for (const l of [...stderr, ...lines]) assert.ok(!l.includes(hex), `the key is in ${l}`);
});

test('without a key nothing changes: one stdin line, no flag', async () => {
  for (const dataKey of [null, () => null, () => Buffer.alloc(16)]) {
    const { got } = await runService(dataKey);
    assert.ok(!got.args.includes('-datakey-stdin'));
    assert.equal(got.lines.length, 1);
  }
});

test('with the Keychain unavailable the service gets -datakey-stdin and "unavailable"', async () => {
  const { got } = await runService(() => UNAVAILABLE);
  assert.ok(got.args.includes('-datakey-stdin'));
  assert.equal(got.lines.length, 2);
  assert.equal(got.lines[1], 'unavailable');
});

test('the default asks datakey.js, which sends nothing outside macOS', async () => {
  if (process.platform === 'darwin') return; // plain Node on a Mac: no Electron API, also null
  const { got } = await runService(undefined);
  assert.ok(!got.args.includes('-datakey-stdin'));
});
