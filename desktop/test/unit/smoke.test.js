// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// scripts/smoke.js (the packaged build's smoke test, release.yml): which
// update mechanism desktop.log must name, and the app's environment.
'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const { updateMechanism, expectedUpdates, smokeEnv } = require('../../scripts/smoke');
const { updateFeedNote } = require('../../src/main/lib/updatefeed');

const READY = '2026-10-27T16:00:09.408Z INFO service ready on port 51247 (attempt 1)\n';
// The lines as the shell writes them (updatepath.js; updatefeed.js updateFeedNote).
const SELF_LINE = 'updates: in-app updates (signed update.json) are configured; electron-updater is not loaded';
const SELF = `${READY}2026-10-27T16:03:01.233Z INFO ${SELF_LINE}\n`;
const NONE = `${READY}2026-10-27T16:00:09.431Z INFO ${updateFeedNote({ enabled: false, reason: 'not_configured' })}\n`;
const REPO = `${READY}2026-10-27T16:00:09.431Z INFO ${updateFeedNote({ enabled: true, owner: 'example-owner', repo: 'example-repo' })}\n`;
const UNUSABLE = `${READY}2026-10-27T16:00:09.431Z INFO ${updateFeedNote({ enabled: false, reason: 'invalid', detail: 'provider "s3" is not github' })}\n`;
// A minisign public key line (made up for the test: "Ed", key id, 32 bytes).
const TEST_KEY = Buffer.concat([Buffer.from('Ed'), Buffer.from('0807060504030201', 'hex'), Buffer.alloc(32, 7)]).toString('base64');
const keysGo = (key) => `package selfupdate\n\nconst PublicKey = "${key}"\n`;
const SELF_KEYS = keysGo(TEST_KEY);
const EMPTY_KEYS = keysGo('');

test('smoke: the in-app update line is the one updatepath.js writes', () => {
  const src = fs.readFileSync(path.join(__dirname, '..', '..', 'src', 'main', 'updatepath.js'), 'utf8');
  assert.ok(src.includes(`'${SELF_LINE}'`), 'updatepath.js logs a different line: update scripts/smoke.js UPDATE_LINES.selfupdate');
});

test('smoke: the expected mechanism follows keys.go, or --expect-updates', () => {
  assert.equal(expectedUpdates(undefined, '/repo', () => SELF_KEYS), 'selfupdate');
  assert.equal(expectedUpdates(undefined, '/repo', () => EMPTY_KEYS), 'electron-updater');
  assert.equal(expectedUpdates('electron-updater', '/repo', () => SELF_KEYS), 'electron-updater', 'the flag wins');
  assert.equal(expectedUpdates('selfupdate', '/repo', () => { throw new Error('not read'); }), 'selfupdate', 'keys.go not read with the flag');
  for (const bad of ['', 'in-app', 'SELFUPDATE', 'any']) {
    assert.throws(() => expectedUpdates(bad, '/repo', () => SELF_KEYS), /--expect-updates/, JSON.stringify(bad));
  }
  assert.throws(() => expectedUpdates(undefined, '/repo', () => { throw new Error('ENOENT'); }), /ENOENT/, 'an unreadable keys.go fails');
  assert.throws(() => expectedUpdates(undefined, '/repo', () => 'package selfupdate\n'), /no line const PublicKey/);
  // This checkout: the release builds carry the maintainer's key (update-key, 2026-10-27).
  const repoKeys = fs.readFileSync(path.join(__dirname, '..', '..', '..', 'internal', 'selfupdate', 'keys.go'), 'utf8');
  assert.equal(expectedUpdates(undefined), /^const PublicKey = ""/m.test(repoKeys) ? 'electron-updater' : 'selfupdate');
});

test('smoke: no update line yet → keep waiting', () => {
  for (const expect of ['selfupdate', 'electron-updater']) {
    assert.equal(updateMechanism('', { updatesOn: false, expect }), null);
    assert.equal(updateMechanism(READY, { updatesOn: true, expect }), null);
    assert.equal(updateMechanism(null, { updatesOn: false, expect }), null);
  }
  assert.throws(() => updateMechanism(SELF, { updatesOn: false }), /expect must be one of/);
});

test('smoke: a build with the built-in update key passes with or without LECTUREFLOW_UPDATE_REPO', () => {
  // pack-dry 2026-10-27: with the key the shell never loads electron-updater,
  // and the old check (only the electron-updater lines) timed out.
  for (const updatesOn of [false, true]) {
    assert.deepEqual(updateMechanism(SELF, { updatesOn, expect: 'selfupdate' }), { mechanism: 'selfupdate', problem: '' }, `updatesOn ${updatesOn}`);
  }
  assert.match(updateMechanism(SELF + NONE, { updatesOn: false, expect: 'selfupdate' }).problem, /names both the in-app update and electron-updater/);
  // The key went missing from a build that should have it: loud.
  assert.match(updateMechanism(NONE, { updatesOn: false, expect: 'selfupdate' }).problem, /no built-in update key.*expected in-app updates/);
  assert.match(updateMechanism(REPO, { updatesOn: true, expect: 'selfupdate' }).problem, /no built-in update key.*expected in-app updates/);
});

test('smoke: without the key the electron-updater line must match LECTUREFLOW_UPDATE_REPO', () => {
  const expect = 'electron-updater';
  assert.deepEqual(updateMechanism(NONE, { updatesOn: false, expect }), { mechanism: 'electron-updater', problem: '' });
  assert.deepEqual(updateMechanism(REPO, { updatesOn: true, expect }), { mechanism: 'electron-updater', problem: '' });
  assert.match(updateMechanism(NONE, { updatesOn: true, expect }).problem, /no update repository is configured, but LECTUREFLOW_UPDATE_REPO is set/);
  assert.match(updateMechanism(REPO, { updatesOn: false, expect }).problem, /an update repository is configured, but LECTUREFLOW_UPDATE_REPO is not set/);
  assert.match(updateMechanism(NONE + REPO, { updatesOn: true, expect }).problem, /both that an update repository is configured and that none is/);
  assert.match(updateMechanism(UNUSABLE, { updatesOn: true, expect }).problem, /not usable/, 'fails at once instead of timing out');
  assert.match(updateMechanism(SELF, { updatesOn: false, expect }).problem, /has a built-in update key.*expected electron-updater/);
  // Both problems are reported together.
  assert.match(updateMechanism(REPO, { updatesOn: false, expect: 'selfupdate' }).problem, /LECTUREFLOW_UPDATE_REPO is not set; .*expected in-app updates/);
});

test('smoke: the app gets its own data dir and no browser extension endpoint', () => {
  const saved = { ...process.env };
  try {
    Object.assign(process.env, {
      LECTUREFLOW_SERVER: 'x.js', LECTUREFLOW_RESOURCES: 'r', ELECTRON_RUN_AS_NODE: '1', LECTUREFLOW_E2E_ALLOW_DEBUG: '1',
    });
    delete process.env.LECTUREFLOW_NM_ENDPOINT;
    const dir = path.join(path.parse(process.cwd()).root, 'lf-smoke-x');
    const env = smokeEnv(dir);
    assert.equal(env.LECTUREFLOW_DATA_DIR, dir);
    // Off: the service would otherwise register the browsers' native
    // messaging host of this temporary build over an installed LectureFlow's.
    assert.equal(env.LECTUREFLOW_NM_ENDPOINT, 'off');
    for (const k of ['LECTUREFLOW_SERVER', 'LECTUREFLOW_RESOURCES', 'ELECTRON_RUN_AS_NODE', 'LECTUREFLOW_E2E_ALLOW_DEBUG']) assert.equal(env[k], undefined, k);
    process.env.LECTUREFLOW_NM_ENDPOINT = '\\\\.\\pipe\\lf-smoke-test';
    assert.equal(smokeEnv(dir).LECTUREFLOW_NM_ENDPOINT, '\\\\.\\pipe\\lf-smoke-test', 'an endpoint the caller chose is kept');
  } finally {
    for (const k of Object.keys(process.env)) if (!(k in saved)) delete process.env[k];
    Object.assign(process.env, saved);
  }
});
