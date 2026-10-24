// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Update policy and relay (SPEC-v3 V8): downloads only on request, installs
// only on quit after the service has stopped, channel from the settings.
'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const { EventEmitter } = require('node:events');

const { UpdateController, prereleaseFor, shouldDownload, installOnQuit, CHECK_INTERVAL_MS } = require('../../src/main/updates');

class FakeUpdater extends EventEmitter {
  constructor({ version = '3.1.0', failDownload = false } = {}) {
    super();
    this.version = version;
    this.failDownload = failDownload;
    this.calls = [];
  }

  async checkForUpdates() {
    this.calls.push(['check', this.allowPrerelease]);
    if (this.version) this.emit('update-available', { version: this.version });
    else this.emit('update-not-available', {});
  }

  async downloadUpdate() {
    this.calls.push(['download']);
    if (this.failDownload) {
      this.emit('error', new Error('network'));
      throw new Error('network');
    }
    this.emit('download-progress', { percent: 50 });
    this.emit('update-downloaded', { version: this.version });
  }

  quitAndInstall(silent, runAfter) {
    this.calls.push(['install', silent, runAfter]);
  }
}

// The relay as the Go service keeps it (internal/api/handlers_update.go).
function fakeApi(settings = { update_channel: 'stable' }) {
  const api = {
    reports: [],
    remote: { state: 'none', download_requested: false, install_on_quit: false },
    settings: async () => settings,
    getUpdate: async () => ({ ...api.remote }),
    postUpdateState: async (body) => {
      api.reports.push(body);
      const same = api.remote.version === body.version;
      api.remote = {
        state: body.state,
        version: body.version || (body.state === 'error' ? api.remote.version : undefined),
        download_requested: body.state === 'available' && same && api.remote.download_requested,
        install_on_quit: body.state !== 'none' && same && api.remote.install_on_quit,
      };
      return api.remote;
    },
  };
  return api;
}

const noTimers = { setInterval: () => 1, clearInterval: () => {} };
const flush = () => new Promise((r) => setImmediate(r));

test('policy functions', () => {
  assert.equal(prereleaseFor({ update_channel: 'beta' }), true);
  assert.equal(prereleaseFor({ update_channel: 'stable' }), false);
  assert.equal(prereleaseFor(null), false);
  assert.equal(shouldDownload({ state: 'available' }, { download_requested: true }), true);
  assert.equal(shouldDownload({ state: 'available' }, { download_requested: false }), false, 'only after the user asked');
  assert.equal(shouldDownload({ state: 'available', busy: 'check' }, { download_requested: true }), false);
  assert.equal(shouldDownload({ state: 'error', version: '3.1.0', failed: 'download' }, { download_requested: true }), true);
  assert.equal(shouldDownload({ state: 'error', version: '', failed: 'check' }, { download_requested: true }), false);
  assert.equal(shouldDownload({ state: 'ready' }, { download_requested: true }), false);
  assert.equal(installOnQuit({ state: 'ready', requested: true, serviceStopped: true }), true);
  assert.equal(installOnQuit({ state: 'ready', requested: true, serviceStopped: false }), false, 'never while the service (and its tasks) still runs');
  assert.equal(installOnQuit({ state: 'ready', requested: false, serviceStopped: true }), false, 'only when the user asked');
  assert.equal(installOnQuit({ state: 'downloading', requested: true, serviceStopped: true }), false);
  assert.equal(CHECK_INTERVAL_MS, 6 * 60 * 60 * 1000);
});

test('start: no automatic download or install; checks now and every 6 hours with the channel', async () => {
  const updater = new FakeUpdater();
  const api = fakeApi({ update_channel: 'beta' });
  const intervals = [];
  const c = new UpdateController({ updater, api, timers: { setInterval: (fn, ms) => { intervals.push(ms); return 7; }, clearInterval: () => {} } });
  c.start();
  await flush();
  assert.equal(updater.autoDownload, false);
  assert.equal(updater.autoInstallOnAppQuit, false);
  assert.deepEqual(intervals, [CHECK_INTERVAL_MS]);
  assert.deepEqual(updater.calls, [['check', true]], 'beta channel allows pre-releases');
  assert.deepEqual(api.reports, [{ state: 'available', version: '3.1.0' }]);
  assert.equal(c.state, 'available');
});

test('download on request, install on quit only after the user asked and the service stopped', async () => {
  const updater = new FakeUpdater();
  const api = fakeApi();
  const c = new UpdateController({ updater, api, timers: noTimers });
  c.start();
  await flush();
  await c.sync();
  assert.deepEqual(updater.calls, [['check', false]], 'nothing downloaded without a request');

  api.remote.download_requested = true; // the page: POST /api/app/update/download
  await c.sync();
  await c.reporting;
  assert.deepEqual(updater.calls.map((x) => x[0]), ['check', 'download']);
  assert.equal(c.state, 'ready');
  assert.deepEqual(api.reports.map((r) => r.state), ['available', 'downloading', 'downloading', 'ready']);

  assert.equal(c.quitAndInstall({ serviceStopped: true }), false, 'not asked to install');
  api.remote.install_on_quit = true; // POST /api/app/update/install-on-quit
  await c.sync();
  assert.equal(c.quitAndInstall({ serviceStopped: false }), false, 'tasks may still run');
  assert.equal(c.quitAndInstall({ serviceStopped: true }), true);
  assert.deepEqual(updater.calls.at(-1), ['install', true, false]);

  // Checks are skipped once the update is downloaded.
  await c.check();
  assert.equal(updater.calls.filter((x) => x[0] === 'check').length, 1);
});

test('failures are reported with their step; a failed download can be asked for again', async () => {
  const updater = new FakeUpdater({ failDownload: true });
  const api = fakeApi();
  const c = new UpdateController({ updater, api, timers: noTimers });
  c.start();
  await flush();
  api.remote.download_requested = true;
  await c.sync();
  await c.reporting;
  assert.equal(c.state, 'error');
  assert.deepEqual(api.reports.at(-1), { state: 'error', version: '3.1.0', error: { code: 'update.download_failed' } });
  updater.failDownload = false;
  api.remote.download_requested = true;
  await c.sync();
  assert.equal(c.state, 'ready');

  const broken = new FakeUpdater();
  broken.checkForUpdates = async () => { throw new Error('offline'); };
  const api2 = fakeApi();
  const c2 = new UpdateController({ updater: broken, api: api2, timers: noTimers });
  c2.start();
  await flush();
  await c2.reporting;
  assert.deepEqual(api2.reports.at(-1), { state: 'error', error: { code: 'update.check_failed' } });
});

test('after a service restart the empty relay gets the state again', async () => {
  const updater = new FakeUpdater();
  const api = fakeApi();
  const c = new UpdateController({ updater, api, timers: noTimers });
  c.start();
  await flush();
  api.remote = { state: 'none', download_requested: false, install_on_quit: false }; // new service process
  await c.sync();
  await c.reporting;
  assert.deepEqual(api.reports.at(-1), { state: 'available', version: '3.1.0' });
});
