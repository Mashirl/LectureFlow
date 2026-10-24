// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Port memory and "use the default location" (v1-shell follow-ups).
'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const net = require('node:net');
const os = require('node:os');
const path = require('node:path');

const { readPortMemo, writePortMemo, makePortPicker } = require('../../src/main/lib/portmemo');
const { backupName, setLocationAside } = require('../../src/main/lib/location');
const { isPortFree, pickFreePort } = require('../../src/main/server');

function tmp() {
  return fs.mkdtempSync(path.join(os.tmpdir(), 'lf-fu-'));
}

test('port memo: written atomically, read back, junk ignored', () => {
  const dir = tmp();
  try {
    const f = path.join(dir, 'sub', 'port.json');
    assert.equal(readPortMemo(f), null);
    assert.equal(writePortMemo(f, 51234), true);
    assert.equal(readPortMemo(f), 51234);
    assert.equal(fs.existsSync(`${f}.tmp`), false);
    assert.equal(writePortMemo(f, 80), false, 'privileged ports are not remembered');
    assert.equal(writePortMemo(f, 70000), false);
    for (const junk of ['{', '[]', '{"port":"51234"}', '{"port":0}', '{"port":65536}', '']) {
      assert.equal(readPortMemo('x', () => junk), null, junk);
    }
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

test('port picker: remembered port first, random when taken or on retries', async () => {
  const calls = [];
  const pick = makePortPicker({
    preferred: 50001,
    isFree: async (p) => { calls.push(['free?', p]); return true; },
    pickFree: async () => { calls.push(['random']); return 60000 + calls.length; },
  });
  assert.equal(await pick(), 50001);
  assert.ok((await pick()) >= 60000, 'a retry after "port in use" takes a random port');
  const taken = makePortPicker({ preferred: 50001, isFree: async () => false, pickFree: async () => 61000 });
  assert.equal(await taken(), 61000);
  const none = makePortPicker({ preferred: null, isFree: async () => { throw new Error('not asked'); }, pickFree: async () => 62000 });
  assert.equal(await none(), 62000);
});

test('isPortFree tells a listening port from a free one', async () => {
  const srv = net.createServer();
  await new Promise((r) => srv.listen(0, '127.0.0.1', r));
  const { port } = srv.address();
  try {
    assert.equal(await isPortFree(port), false);
  } finally {
    await new Promise((r) => srv.close(r));
  }
  assert.equal(await isPortFree(port), true);
  const p = await pickFreePort();
  assert.ok(p > 0);
});

test('location.json is renamed, never deleted, and never overwrites an older backup', () => {
  const dir = tmp();
  try {
    const loc = path.join(dir, 'location.json');
    fs.writeFileSync(loc, '{broken');
    const first = setLocationAside(loc);
    assert.equal(first, `${loc}.bak`);
    assert.equal(fs.readFileSync(first, 'utf8'), '{broken');
    assert.equal(fs.existsSync(loc), false);
    fs.writeFileSync(loc, '{"data_dir": 5}');
    const second = setLocationAside(loc, { now: new Date(2026, 9, 24, 9, 5, 7) });
    assert.equal(second, `${loc}.bak-20261024-090507`);
    assert.equal(fs.readFileSync(first, 'utf8'), '{broken', 'the first backup is kept');
    assert.deepEqual(fs.readdirSync(dir).sort(), ['location.json.bak', 'location.json.bak-20261024-090507']);
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
  const taken = new Set(['L.bak', 'L.bak-20260101-000000']);
  assert.equal(backupName('L', (p) => taken.has(p), new Date(2026, 0, 1, 0, 0, 0)), 'L.bak-20260101-000000-1');
});
