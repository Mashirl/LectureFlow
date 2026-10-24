// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The packaged app refuses debugging switches (acceptance V12.2 finding 11).
'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const { debugSwitchesIn, refuseDebugging, DEBUG_SWITCHES } = require('../../src/main/lib/debugflags');

test('debugging switches on a command line, in every spelling Chromium accepts', () => {
  assert.deepEqual(debugSwitchesIn(['--remote-debugging-port=9222'], 'win32'), ['remote-debugging-port']);
  assert.deepEqual(debugSwitchesIn(['--remote-debugging-port', '9222'], 'win32'), ['remote-debugging-port']);
  assert.deepEqual(debugSwitchesIn(['-remote-debugging-port=0'], 'win32'), ['remote-debugging-port']);
  assert.deepEqual(debugSwitchesIn(['/remote-debugging-port=0'], 'win32'), ['remote-debugging-port']);
  assert.deepEqual(debugSwitchesIn(['/remote-debugging-port=0'], 'darwin'), [], '"/" is a path outside Windows');
  assert.deepEqual(debugSwitchesIn(['--Remote-Debugging-Pipe'], 'win32'), ['remote-debugging-pipe']);
  assert.deepEqual(debugSwitchesIn(['--inspect=9229', '--inspect-brk', '--lang=en'], 'darwin'), ['inspect', 'inspect-brk']);
  assert.deepEqual(debugSwitchesIn(['--lang=zh-CN', 'D:\\lecture.mp4', '--remote-debugging-portx'], 'win32'), []);
  assert.deepEqual(debugSwitchesIn(['--', '--remote-debugging-port=1'], 'win32'), [], 'after "--" there are only arguments');
  assert.deepEqual(debugSwitchesIn(undefined), []);
});

test('only the packaged app refuses, and LECTUREFLOW_E2E_ALLOW_DEBUG=1 lets the switches through', () => {
  const argv = ['--remote-debugging-port=9222'];
  assert.deepEqual(refuseDebugging({ isPackaged: true, argv, platform: 'win32' }), ['remote-debugging-port']);
  assert.deepEqual(refuseDebugging({ isPackaged: false, argv, platform: 'win32' }), [], 'development and Playwright runs');
  assert.deepEqual(refuseDebugging({ isPackaged: true, argv, env: { LECTUREFLOW_E2E_ALLOW_DEBUG: '1' }, platform: 'win32' }), []);
  for (const v of ['0', 'true', 'yes', '', ' 1']) {
    assert.deepEqual(refuseDebugging({ isPackaged: true, argv, env: { LECTUREFLOW_E2E_ALLOW_DEBUG: v }, platform: 'win32' }), ['remote-debugging-port'], JSON.stringify(v));
  }
  assert.deepEqual(refuseDebugging({ isPackaged: true, argv: [], platform: 'win32' }), []);
});

test("Chromium's own parse of the command line counts too", () => {
  const hasSwitch = (s) => s === 'remote-debugging-pipe';
  assert.deepEqual(refuseDebugging({ isPackaged: true, argv: [], hasSwitch, platform: 'win32' }), ['remote-debugging-pipe']);
  assert.deepEqual(refuseDebugging({ isPackaged: true, argv: [], hasSwitch: () => { throw new Error('x'); }, platform: 'win32' }), []);
  assert.ok(DEBUG_SWITCHES.includes('remote-debugging-port') && DEBUG_SWITCHES.includes('inspect-brk'));
});

test('main.js checks before anything else of the app starts', () => {
  const src = fs.readFileSync(path.join(__dirname, '..', '..', 'src', 'main', 'main.js'), 'utf8');
  const body = src.slice(src.indexOf('function main() {'));
  assert.match(body, /^function main\(\) \{\n {2}if \(refuseDebugSwitches\(\)\) return;\n/);
});
