// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
'use strict';

// datadir-ui (security review sec-update M2): the shell never creates the
// data dir — the service does, private from the start (internal/dirperm) —
// so its log stays in the profile until the data dir exists.

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');

const { createLogger, shellLogFile, mkdirLeaf } = require('../../src/main/logger');

const SHELL = path.join(__dirname, '..', '..', 'src', 'main', 'shell.js');

function tmp() {
  return fs.mkdtempSync(path.join(os.tmpdir(), 'lf-datadirperm-'));
}

const isDir = (p) => {
  try {
    return fs.statSync(p).isDirectory();
  } catch {
    return false;
  }
};

test('the logger creates its logs folder, never a missing data dir above it', () => {
  const root = tmp();
  try {
    const dataDir = path.join(root, 'LectureFlow');
    const file = path.join(dataDir, 'logs', 'desktop.log');
    const log = createLogger({ file });
    log.info('before the service ran');
    assert.equal(log.file, null, 'no target while the data dir is missing');
    assert.equal(fs.existsSync(dataDir), false, 'the data dir was not created');

    fs.mkdirSync(dataDir); // the service creates it
    log.setFile(file);
    log.info('after');
    assert.equal(log.file, file);
    assert.match(fs.readFileSync(file, 'utf8'), /INFO after/);
    // an existing logs folder is fine
    log.setFile(file);
    assert.equal(log.file, file);
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
});

test('mkdirLeaf: the folder itself, an existing one, never its parents', () => {
  const root = tmp();
  try {
    const leaf = path.join(root, 'logs');
    mkdirLeaf(leaf);
    assert.equal(isDir(leaf), true);
    mkdirLeaf(leaf); // EEXIST is fine
    assert.throws(() => mkdirLeaf(path.join(root, 'missing', 'logs')), { code: 'ENOENT' });
    assert.equal(fs.existsSync(path.join(root, 'missing')), false);
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
});

test('shellLogFile: the data dir once it exists, the profile until then', () => {
  const dirs = new Set(['D:\\LectureFlow']);
  const w = (dataDir) => shellLogFile({ dataDir, profileDir: 'C:\\AppData\\LectureFlow\\electron', isDir: (p) => dirs.has(p), pathMod: path.win32 });
  assert.equal(w('D:\\LectureFlow'), 'D:\\LectureFlow\\logs\\desktop.log');
  assert.equal(w('E:\\New'), 'C:\\AppData\\LectureFlow\\electron\\logs\\desktop.log');
  assert.equal(w(null), 'C:\\AppData\\LectureFlow\\electron\\logs\\desktop.log');
});

test('the shell leaves creating the data dir to the service (source)', () => {
  const src = fs.readFileSync(SHELL, 'utf8');
  const start = src.slice(src.indexOf('async startServer('), src.indexOf('// ---- in-app update'));
  assert.ok(start.length > 1000, 'startServer found');
  assert.doesNotMatch(start, /mkdirSync|mkdir\(/, 'startServer creates no folder');
  // the log: profile until the data dir exists, then next to the service's
  const first = start.indexOf('this.log.setFile(shellLogFile(');
  const started = start.indexOf('await server.start()');
  assert.ok(first > 0 && first < started, 'the log is placed before the service starts');
  assert.ok(start.indexOf('this.log.setFile(shellLogFile(', started) > started, 'and placed again once it runs');
  assert.match(start, /if \(dataDirMissing && !isDir\(dataDir\)\) page\.detail = \[this\.t\('reason\.unwritable'/);
  // "Open logs" opens the log's folder without creating the data dir
  const openLogs = src.slice(src.indexOf('  openLogs() {'), src.indexOf('  openExternal('));
  assert.match(openLogs, /path\.dirname\(this\.log\.file\)/);
  assert.match(openLogs, /mkdirLeaf\(dir\)/);
  assert.doesNotMatch(openLogs, /recursive/);
});
