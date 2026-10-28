// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
'use strict';

// No spellchecker dictionary downloads (privacy audit before 3.2.0, NOTES.md
// "无遥测"): with the sandbox on, Electron fetched en-US-10-1.bdic from
// redirector.gvt1.com for the default session at every start.

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { EventEmitter } = require('node:events');

const { disableSpellcheck, noSpellcheckDownloads } = require('../../src/main/websecurity');

function fakeSession({ throwLanguages = false } = {}) {
  const calls = [];
  return {
    calls,
    setSpellCheckerEnabled: (v) => calls.push(['enabled', v]),
    setSpellCheckerLanguages: (v) => {
      if (throwLanguages) throw new Error('not supported');
      calls.push(['languages', v]);
    },
  };
}

test('disableSpellcheck: off and no languages; a missing API is no error', () => {
  const s = fakeSession();
  disableSpellcheck(s);
  assert.deepEqual(s.calls, [['enabled', false], ['languages', []]]);
  const mac = fakeSession({ throwLanguages: true });
  disableSpellcheck(mac);
  assert.deepEqual(mac.calls, [['enabled', false]]);
});

test('noSpellcheckDownloads: every session Electron creates, the default one included', () => {
  const app = new EventEmitter();
  noSpellcheckDownloads(app);
  const def = fakeSession();
  const part = fakeSession();
  app.emit('session-created', def);
  app.emit('session-created', part);
  for (const s of [def, part]) assert.deepEqual(s.calls, [['enabled', false], ['languages', []]]);
});

test('main.js installs it right after enabling the sandbox, before the app is ready', () => {
  const src = fs.readFileSync(path.join(__dirname, '..', '..', 'src', 'main', 'main.js'), 'utf8');
  const sandbox = src.indexOf('app.enableSandbox();');
  const install = src.indexOf('noSpellcheckDownloads(app);');
  const ready = src.indexOf('app.whenReady()');
  assert.ok(sandbox > 0 && install > sandbox && install < ready, 'noSpellcheckDownloads(app) follows app.enableSandbox() before app.whenReady()');
});
