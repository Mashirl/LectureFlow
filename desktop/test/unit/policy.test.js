// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');

const { decideClose, interpretCloseDialog, wantSleepBlock, runningCount } = require('../../src/main/lib/policy');
const { SleepGuard, BLOCK_TYPE } = require('../../src/main/sleepguard');

const act = (running, queued = 0) => ({ running, queued, transcribing: running > 0, api_running: false });

test('close without running tasks: quit on Windows, stay in the Dock on macOS', () => {
  for (const settings of [null, { close_behavior: 'ask' }, { close_behavior: 'background' }, { close_behavior: 'quit' }]) {
    assert.equal(decideClose({ platform: 'win32', activity: act(0, 3), settings }), 'quit');
    assert.equal(decideClose({ platform: 'darwin', activity: act(0), settings }), 'hide');
    assert.equal(decideClose({ platform: 'win32', activity: null, settings }), 'quit', 'service unreachable');
  }
});

test('close with running tasks follows close_behavior', () => {
  for (const platform of ['win32', 'darwin']) {
    assert.equal(decideClose({ platform, activity: act(2), settings: { close_behavior: 'ask' } }), 'ask');
    assert.equal(decideClose({ platform, activity: act(1), settings: { close_behavior: 'background' } }), 'background');
    assert.equal(decideClose({ platform, activity: act(1), settings: { close_behavior: 'quit' } }), 'quit');
    assert.equal(decideClose({ platform, activity: act(1), settings: null }), 'ask', 'unknown settings ask');
    assert.equal(decideClose({ platform, activity: act(1), settings: { close_behavior: 'bogus' } }), 'ask');
  }
});

test('close dialog: buttons and "remember my choice"', () => {
  assert.deepEqual(interpretCloseDialog({ response: 0, checkboxChecked: false }), { action: 'background', remember: null });
  assert.deepEqual(interpretCloseDialog({ response: 0, checkboxChecked: true }), { action: 'background', remember: 'background' });
  assert.deepEqual(interpretCloseDialog({ response: 1, checkboxChecked: true }), { action: 'quit', remember: 'quit' });
  assert.deepEqual(interpretCloseDialog({ response: 2, checkboxChecked: true }), { action: 'cancel', remember: null }, 'cancel never saves');
  assert.deepEqual(interpretCloseDialog({ response: 7 }), { action: 'cancel', remember: null });
  assert.deepEqual(interpretCloseDialog(null), { action: 'cancel', remember: null });
});

test('sleep is blocked only while tasks run and the setting allows it', () => {
  assert.equal(wantSleepBlock(act(1), { prevent_sleep: true }), true);
  assert.equal(wantSleepBlock(act(1), null), true, 'default on');
  assert.equal(wantSleepBlock(act(1), { prevent_sleep: false }), false);
  assert.equal(wantSleepBlock(act(0, 5), { prevent_sleep: true }), false, 'queued only');
  assert.equal(wantSleepBlock(null, { prevent_sleep: true }), false);
  assert.equal(runningCount({ running: -1 }), 0);
  assert.equal(runningCount({ running: '3' }), 0);
});

test('a device self-test (transcribing, no running job) blocks sleep too, under prevent_sleep', () => {
  const selfTest = { running: 0, queued: 0, transcribing: true, api_running: false };
  assert.equal(wantSleepBlock(selfTest, { prevent_sleep: true }), true);
  assert.equal(wantSleepBlock(selfTest, null), true, 'default on');
  assert.equal(wantSleepBlock(selfTest, {}), true, 'setting missing: default on');
  assert.equal(wantSleepBlock(selfTest, { prevent_sleep: false }), false);
  assert.equal(wantSleepBlock({ ...selfTest, queued: 4 }, { prevent_sleep: false }), false);
  // not transcribing and nothing running: no block
  assert.equal(wantSleepBlock({ running: 0, queued: 2, transcribing: false, api_running: true }, { prevent_sleep: true }), false);
  assert.equal(wantSleepBlock({ running: 0 }, { prevent_sleep: true }), false, 'transcribing absent');
  // the self-test is not a running task: closing is unchanged
  assert.equal(decideClose({ platform: 'win32', activity: selfTest, settings: { close_behavior: 'ask' } }), 'quit');
  assert.equal(runningCount(selfTest), 0);
});

test('SleepGuard starts once, stops when no longer wanted', () => {
  const calls = [];
  let next = 7;
  const started = new Set();
  const blocker = {
    start: (type) => { calls.push(['start', type]); started.add(next); return next++; },
    stop: (id) => { calls.push(['stop', id]); started.delete(id); },
    isStarted: (id) => started.has(id),
  };
  const g = new SleepGuard({ blocker });
  g.update(false);
  g.update(true);
  g.update(true);
  assert.equal(g.active, true);
  g.update(false);
  g.update(false);
  assert.equal(g.active, false);
  g.update(true);
  g.release();
  g.release();
  assert.deepEqual(calls, [['start', BLOCK_TYPE], ['stop', 7], ['start', BLOCK_TYPE], ['stop', 8]]);
  assert.equal(BLOCK_TYPE, 'prevent-app-suspension');
});
