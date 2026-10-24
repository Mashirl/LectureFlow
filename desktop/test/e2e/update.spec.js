// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The update relay end to end (SPEC-v3 V8), against the real service's
// /api/app/update endpoints and a stand-in for electron-updater
// (test/fixtures/fake-updater.js): found, downloaded on request, installed
// when the app quits and only when asked.
'use strict';

const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { test, expect, waitForApp, DESKTOP } = require('./fixtures');

const FAKE_UPDATER = path.join(DESKTOP, 'test', 'fixtures', 'fake-updater.js');

function markFile(tag) {
  return path.join(os.tmpdir(), `lf-update-mark-${process.pid}-${Date.now()}-${tag}.json`);
}

async function launchWithUpdate(launch, mark) {
  const r = await launch({ env: { LECTUREFLOW_FAKE_UPDATER: FAKE_UPDATER, LECTUREFLOW_FAKE_UPDATER_MARK: mark } });
  await waitForApp(r.page);
  const status = () => r.page.evaluate(async () => (await fetch('/api/app/update')).json());
  const post = (p, body) => r.page.evaluate(async ([url, b]) => (await fetch(url, {
    method: 'POST', body: b, headers: b ? { 'Content-Type': 'application/json' } : {},
  })).status, [p, body]);
  return { ...r, status, post };
}

test('update: found, downloaded on request, installed on quit when asked', async ({ launch }) => {
  test.setTimeout(120_000);
  const mark = markFile('a');
  const { app, status, post, page } = await launchWithUpdate(launch, mark);
  await expect.poll(status, { timeout: 15_000 }).toMatchObject({ state: 'available', version: '9.9.9' });

  // The page asks; the main process acts on it within one 5-second poll.
  expect(await post('/api/app/update/download')).toBe(202);
  await expect.poll(status, { timeout: 15_000 }).toMatchObject({ state: 'ready', version: '9.9.9', percent: 100, download_requested: false });

  expect(await post('/api/app/update/install-on-quit', '{"enabled":true}')).toBe(200);
  await expect.poll(status).toMatchObject({ install_on_quit: true });
  await page.waitForTimeout(6000); // one poll of the main process
  expect(fs.existsSync(mark)).toBe(false); // nothing is installed while the app runs
  await app.close();
  const m = JSON.parse(fs.readFileSync(mark, 'utf8'));
  fs.rmSync(mark, { force: true });
  expect(m).toMatchObject({ isSilent: true, isForceRunAfter: false });
  expect(m.checks[0]).toEqual({ allowPrerelease: false }); // stable channel by default
});

test('update: a downloaded update is not installed when the user did not ask', async ({ launch }) => {
  test.setTimeout(90_000);
  const mark = markFile('b');
  const { app, status, post } = await launchWithUpdate(launch, mark);
  await expect.poll(status, { timeout: 15_000 }).toMatchObject({ state: 'available' });
  expect(await post('/api/app/update/download')).toBe(202);
  await expect.poll(status, { timeout: 15_000 }).toMatchObject({ state: 'ready' });
  await app.close();
  expect(fs.existsSync(mark)).toBe(false);
});
