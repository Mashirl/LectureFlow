// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const path = require('node:path');

const { resolveDataDir, parseLocation } = require('../../src/main/lib/datadir');

const enoent = () => {
  const e = new Error('missing');
  e.code = 'ENOENT';
  throw e;
};
const file = (content) => () => content;

test('Windows default: %APPDATA%\\LectureFlow, Chromium profile in a subfolder', () => {
  const r = resolveDataDir({ env: {}, appData: 'C:\\Users\\s\\AppData\\Roaming', readFile: enoent, pathMod: path.win32 });
  assert.equal(r.source, 'default');
  assert.equal(r.dataDir, 'C:\\Users\\s\\AppData\\Roaming\\LectureFlow');
  assert.equal(r.defaultRoot, r.dataDir);
  assert.equal(r.profileDir, 'C:\\Users\\s\\AppData\\Roaming\\LectureFlow\\electron');
  assert.equal(r.locationFile, 'C:\\Users\\s\\AppData\\Roaming\\LectureFlow\\location.json');
  assert.equal(r.error, undefined);
});

test('macOS default: ~/Library/Application Support/LectureFlow', () => {
  const r = resolveDataDir({ env: {}, appData: '/Users/s/Library/Application Support', readFile: enoent, pathMod: path.posix });
  assert.equal(r.dataDir, '/Users/s/Library/Application Support/LectureFlow');
  assert.equal(r.source, 'default');
});

test('LECTUREFLOW_DATA_DIR wins over location.json and the default', () => {
  let read = false;
  const r = resolveDataDir({
    env: { LECTUREFLOW_DATA_DIR: '  D:\\lf-test  ' },
    appData: 'C:\\AppData',
    readFile: () => { read = true; return '{"data_dir":"E:\\\\elsewhere"}'; },
    pathMod: path.win32,
  });
  assert.equal(r.source, 'env');
  assert.equal(r.dataDir, 'D:\\lf-test');
  assert.equal(r.profileDir, 'D:\\lf-test\\electron');
  assert.equal(read, false);
});

test('location.json moves the data dir', () => {
  const r = resolveDataDir({ env: {}, appData: 'C:\\AppData', readFile: file('\uFEFF{"data_dir": "E:\\\\Lectures\\\\LF\\\\"}'), pathMod: path.win32 });
  assert.equal(r.source, 'location');
  assert.equal(r.dataDir, 'E:\\Lectures\\LF\\');
  assert.equal(r.profileDir, 'C:\\AppData\\LectureFlow\\electron', 'profile stays at the default root');
});

test('an unusable location.json is an error, never a silent fallback', () => {
  for (const [content, reason] of [
    ['{', 'not_json'],
    ['[]', 'not_object'],
    ['{}', 'no_data_dir'],
    ['{"data_dir": ""}', 'no_data_dir'],
    ['{"data_dir": 5}', 'no_data_dir'],
    ['{"data_dir": "relative\\\\dir"}', 'not_absolute'],
  ]) {
    const r = resolveDataDir({ env: {}, appData: 'C:\\AppData', readFile: file(content), pathMod: path.win32 });
    assert.equal(r.dataDir, null, content);
    assert.deepEqual(r.error, { kind: 'location_invalid', reason }, content);
  }
  const unreadable = resolveDataDir({
    env: {},
    appData: 'C:\\AppData',
    readFile: () => { const e = new Error('denied'); e.code = 'EACCES'; throw e; },
    pathMod: path.win32,
  });
  assert.deepEqual(unreadable.error, { kind: 'location_invalid', reason: 'EACCES' });
});

test('parseLocation accepts absolute POSIX paths on macOS', () => {
  assert.deepEqual(parseLocation('{"data_dir":"/Volumes/Big/LF"}', path.posix), { ok: true, dataDir: '/Volumes/Big/LF' });
  assert.equal(parseLocation('{"data_dir":"Volumes/Big"}', path.posix).ok, false);
  assert.equal(parseLocation(`{"data_dir":"/${'a'.repeat(5000)}"}`, path.posix).ok, false);
});

test('appData is required', () => {
  assert.throws(() => resolveDataDir({ env: {}, appData: '', readFile: enoent }), TypeError);
});
