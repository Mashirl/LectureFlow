// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
'use strict';

// perf-fix: the shell tells the app page whether its window is shown, so
// the service samples the GPU only for a window someone can see (web/api.js
// posts the state; internal/api/visibility.go).

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const { Shell } = require('../../src/main/shell');

const SHELL = path.join(__dirname, '..', '..', 'src', 'main', 'shell.js');

function fakeShell(port = 18999) {
  const sh = Object.create(Shell.prototype);
  sh.server = { ready: true, port };
  return sh;
}

function fakeWindow({ url, visible = true, minimized = false, destroyed = false } = {}) {
  const scripts = [];
  return {
    scripts,
    isDestroyed: () => destroyed,
    isVisible: () => visible,
    isMinimized: () => minimized,
    webContents: {
      getURL: () => url,
      executeJavaScript: (code) => {
        scripts.push(code);
        return Promise.resolve();
      },
    },
  };
}

// The event a script dispatches, run against a fake window object.
function dispatched(code) {
  const events = [];
  const window = { dispatchEvent: (e) => events.push(e) };
  class CustomEvent {
    constructor(type, init) {
      this.type = type;
      this.detail = init && init.detail;
    }
  }
  new Function('window', 'CustomEvent', code)(window, CustomEvent);
  return events;
}

test('a shown window reports visible, a hidden or minimized one not', () => {
  const sh = fakeShell();
  const url = 'http://127.0.0.1:18999/#/';
  for (const [state, want] of [
    [{ visible: true, minimized: false }, true],
    [{ visible: false, minimized: false }, false],
    [{ visible: true, minimized: true }, false],
  ]) {
    const win = fakeWindow({ url, ...state });
    sh._reportVisibility(win);
    assert.equal(win.scripts.length, 1);
    const evs = dispatched(win.scripts[0]);
    assert.equal(evs.length, 1);
    assert.equal(evs[0].type, 'lectureflow:window-visibility');
    assert.deepEqual(evs[0].detail, { visible: want }, JSON.stringify(state));
  }
});

test('nothing is sent to the splash or error page, or a closed window', () => {
  const sh = fakeShell();
  const splash = fakeWindow({ url: 'file:///C:/app/splash.html?state=starting' });
  sh._reportVisibility(splash);
  assert.equal(splash.scripts.length, 0);
  const other = fakeWindow({ url: 'http://127.0.0.1:1/' }); // not the service's port
  sh._reportVisibility(other);
  assert.equal(other.scripts.length, 0);
  const gone = fakeWindow({ url: 'http://127.0.0.1:18999/', destroyed: true });
  sh._reportVisibility(gone);
  assert.equal(gone.scripts.length, 0);
  sh._reportVisibility(null);
  const notReady = Object.create(Shell.prototype);
  notReady.server = { ready: false, port: 18999 };
  const w = fakeWindow({ url: 'http://127.0.0.1:18999/' });
  notReady._reportVisibility(w);
  assert.equal(w.scripts.length, 0);
});

test('createWindow reports on show, hide, minimize, restore and page load (source)', () => {
  const src = fs.readFileSync(SHELL, 'utf8');
  const create = src.slice(src.indexOf('  createWindow() {'), src.indexOf('  _ensureWindow() {'));
  assert.ok(create.length > 500, 'createWindow found');
  assert.match(create, /for \(const name of \['show', 'hide', 'minimize', 'restore'\]\) win\.on\(name, \(\) => this\._reportVisibility\(win\)\);/);
  const load = create.slice(create.indexOf("wc.on('did-finish-load'"), create.indexOf("wc.on('render-process-gone'"));
  assert.match(load, /this\._reportVisibility\(win\)/);
});

test('the page listens for the event when api.js loads (source)', () => {
  const api = fs.readFileSync(path.join(__dirname, '..', '..', '..', 'web', 'api.js'), 'utf8');
  assert.match(api, /window\.addEventListener\('lectureflow:window-visibility'/);
  assert.match(api, /e\.detail\.visible === false/);
});
