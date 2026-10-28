// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Which update mechanism runs (updatepath.js): while the service has no
// minisign public key built in, the electron-updater path of SPEC-v3 V8 stays
// as it was; with a key, the in-app update of SPEC-ext runs and
// electron-updater is never loaded.
'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');

const { UpdatePathChooser, updatePathFor, askService, SELFUPDATE, ELECTRON_UPDATER, MAX_ATTEMPTS } = require('../../src/main/updatepath');
const { ApiError } = require('../../src/main/apiclient');

function chooser(answers, { maxAttempts } = {}) {
  const seen = { loads: 0, controllers: [], logs: [] };
  const api = {
    calls: 0,
    selfUpdate: async () => {
      const a = answers[Math.min(api.calls, answers.length - 1)];
      api.calls++;
      if (a instanceof Error) throw a;
      return a;
    },
  };
  const c = new UpdatePathChooser({
    api,
    loadUpdater: () => {
      seen.loads++;
      return { updater: { fake: true }, note: 'updates: checking GitHub releases of o/r' };
    },
    createController: (updater) => {
      const ctl = { updater, started: 0, start() { this.started++; } };
      seen.controllers.push(ctl);
      return ctl;
    },
    log: { info: (m) => seen.logs.push(m), warn: (m) => seen.logs.push(m) },
    ...(maxAttempts ? { maxAttempts } : {}),
  });
  return { c, api, seen };
}

test('updatePathFor: only "no public key" keeps electron-updater', () => {
  assert.equal(updatePathFor({ state: 'not_configured', reason: 'no_public_key' }), ELECTRON_UPDATER);
  for (const s of [
    { state: 'idle' }, { state: 'pending' }, { state: 'error' }, { state: 'up_to_date' },
    { state: 'not_configured', reason: 'dev_build' }, { state: 'not_configured', reason: 'unsupported_platform' },
  ]) assert.equal(updatePathFor(s), SELFUPDATE, JSON.stringify(s));
  assert.equal(updatePathFor(null), ELECTRON_UPDATER);
  assert.equal(updatePathFor({}), ELECTRON_UPDATER);
});

test('askService: an older service (404) or one without the updater (503) means electron-updater; other errors: ask again', async () => {
  const ask = (x) => askService({ selfUpdate: async () => { if (x instanceof Error) throw x; return x; } });
  assert.equal(await ask(new ApiError(404, 'not found')), ELECTRON_UPDATER);
  assert.equal(await ask(new ApiError(503, 'unavailable')), ELECTRON_UPDATER);
  assert.equal(await ask(new ApiError(500, 'boom')), null);
  assert.equal(await ask(new ApiError(0, 'service not running')), null);
  assert.equal(await ask(new Error('timeout')), null);
  assert.equal(await ask({ state: 'idle' }), SELFUPDATE);
});

test('no public key: electron-updater is loaded and started once, as before', async () => {
  const { c, api, seen } = chooser([{ state: 'not_configured', reason: 'no_public_key', current_version: '3.1.0' }]);
  assert.equal(c.decided, false);
  assert.equal(await c.decide(), ELECTRON_UPDATER);
  assert.equal(c.decided, true);
  assert.equal(seen.loads, 1);
  assert.equal(seen.controllers.length, 1);
  assert.equal(seen.controllers[0].started, 1);
  assert.equal(c.controller, seen.controllers[0]);
  assert.ok(seen.logs.includes('updates: checking GitHub releases of o/r'));
  await c.decide();
  assert.equal(api.calls, 1, 'decided once');
  assert.equal(seen.loads, 1);
});

test('no public key and no update feed in this build: nothing is started', async () => {
  const { c, seen } = chooser([{ state: 'not_configured', reason: 'no_public_key' }]);
  c.loadUpdater = () => {
    seen.loads++;
    return { updater: null, note: 'updates: no update repository was configured for this build; automatic updates are off' };
  };
  assert.equal(await c.decide(), ELECTRON_UPDATER);
  assert.equal(c.controller, null);
  assert.equal(seen.loads, 1);
});

test('a public key is built in: electron-updater is never loaded', async () => {
  const { c, seen } = chooser([{ state: 'idle', current_version: '3.1.0' }]);
  assert.equal(await c.decide(), SELFUPDATE);
  assert.equal(seen.loads, 0);
  assert.equal(c.controller, null);
  assert.match(seen.logs[0], /electron-updater is not loaded/);
});

test('no answer yet: asked again at the next poll; never the previous mechanism by time alone (sec-update)', async () => {
  const { c, api, seen } = chooser([new ApiError(0, 'service not running'), new ApiError(500, 'x'), { state: 'pending' }]);
  assert.equal(await c.decide(), 'undecided');
  assert.equal(await c.decide(), 'undecided');
  assert.equal(await c.decide(), SELFUPDATE);
  assert.equal(api.calls, 3);
  assert.equal(seen.loads, 0);

  // A service that never answers does not switch a build with a public key
  // to electron-updater; it is logged once and asked again at every poll.
  const never = chooser([new ApiError(500, 'x')], { maxAttempts: 3 });
  for (let i = 0; i < 8; i++) assert.equal(await never.c.decide(), 'undecided');
  assert.equal(never.seen.loads, 0);
  assert.equal(never.api.calls, 8);
  assert.equal(never.seen.logs.filter((l) => /did not say whether in-app updates are configured \(3 tries\); no update mechanism runs/.test(l)).length, 1);
  assert.ok(MAX_ATTEMPTS >= 3);
  // ... until it answers
  const late = chooser([new ApiError(500, 'x'), new ApiError(500, 'x'), new ApiError(500, 'x'), new ApiError(500, 'x'), { state: 'not_configured', reason: 'no_public_key' }], { maxAttempts: 2 });
  for (let i = 0; i < 4; i++) await late.c.decide();
  assert.equal(await late.c.decide(), ELECTRON_UPDATER);
  assert.equal(late.seen.loads, 1);
});

test('concurrent polls ask once', async () => {
  const { c, api } = chooser([{ state: 'idle' }]);
  const [a, b] = await Promise.all([c.decide(), c.decide()]);
  assert.equal(a, SELFUPDATE);
  assert.equal(b, SELFUPDATE);
  assert.equal(api.calls, 1);
});

test('the API client asks GET /api/selfupdate with the token', async () => {
  const { createApiClient } = require('../../src/main/apiclient');
  const seen = [];
  const api = createApiClient({
    getServer: () => ({ origin: 'http://127.0.0.1:5000', token: 't'.repeat(64) }),
    fetchImpl: async (url, init) => {
      seen.push({ url, method: init.method, token: init.headers['X-LF-Token'] });
      return { ok: true, status: 200, text: async () => '{"state":"not_configured","reason":"no_public_key"}' };
    },
  });
  assert.deepEqual(await api.selfUpdate(), { state: 'not_configured', reason: 'no_public_key' });
  assert.deepEqual(seen, [{ url: 'http://127.0.0.1:5000/api/selfupdate', method: 'GET', token: 't'.repeat(64) }]);
});
