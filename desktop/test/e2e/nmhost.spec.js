// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The browser extension's side of the shell (SPEC-ext, .claude/ext-brief.md
// §4): started by the native host with --background, the app runs without a
// window; a second --background start keeps it hidden; the extension's
// "open" (a LECTUREFLOW_OPEN line of the service) shows the window on that
// route. Fake service only (it prints the line on /test/open).
'use strict';

const { spawn } = require('node:child_process');
const path = require('node:path');
const { test, expect, waitForApp, request, fakeState, mainWindowVisible, DESKTOP } = require('./fixtures');

const ELECTRON = require(path.join(DESKTOP, 'node_modules', 'electron'));

async function secondStart(env, args) {
  const second = spawn(ELECTRON, [DESKTOP, ...args], { env, stdio: 'ignore' });
  return new Promise((resolve) => second.once('exit', resolve));
}

test('--background starts without a window; open shows it on the route', async ({ launch, serverKind }) => {
  test.skip(serverKind !== 'fake', 'needs the fake service');
  const { app, page, dataDir, env } = await launch({ args: ['--background'] });
  const { port } = await waitForApp(page);
  // The app is loaded but the window stays hidden.
  await new Promise((r) => setTimeout(r, 1000));
  expect(await mainWindowVisible(app)).toBe(false);

  // The native host starting it again (the extension reconnected): still hidden.
  expect(await secondStart(env, ['--background'])).toBe(0);
  await new Promise((r) => setTimeout(r, 1000));
  expect(await mainWindowVisible(app)).toBe(false);
  expect(fakeState(dataDir).attempts).toBe(1);

  // "在 LectureFlow 中打开": shown and on the route.
  const res = await request(port, `/test/open?route=${encodeURIComponent('#/lecture/01JTESTLECTURE')}`);
  expect(res.status).toBe(200);
  await expect.poll(() => mainWindowVisible(app)).toBe(true);
  await expect.poll(() => new URL(page.url()).hash).toBe('#/lecture/01JTESTLECTURE');

  // A route that is not one of the app's is ignored.
  await request(port, `/test/open?route=${encodeURIComponent('javascript:alert(1)')}`);
  await new Promise((r) => setTimeout(r, 500));
  expect(new URL(page.url()).hash).toBe('#/lecture/01JTESTLECTURE');
});

test('open while the window is hidden in the background brings it back', async ({ launch, serverKind }) => {
  test.skip(serverKind !== 'fake', 'needs the fake service');
  const { app, page } = await launch();
  const { port } = await waitForApp(page);
  await expect.poll(() => mainWindowVisible(app)).toBe(true);
  await app.evaluate(({ BrowserWindow }) => BrowserWindow.getAllWindows()[0].hide());
  await expect.poll(() => mainWindowVisible(app)).toBe(false);
  await request(port, `/test/open?route=${encodeURIComponent('#/course/01JTESTCOURSE')}`);
  await expect.poll(() => mainWindowVisible(app)).toBe(true);
  await expect.poll(() => new URL(page.url()).hash).toBe('#/course/01JTESTCOURSE');
});

test('a failed start in the background shows the window', async ({ launch, serverKind }) => {
  test.skip(serverKind !== 'fake', 'needs the fake service');
  const { app, page } = await launch({ args: ['--background'], env: { FAKE_START_FAIL_TIMES: '1' } });
  await expect.poll(() => page.url(), { timeout: 30_000 }).toMatch(/splash\.html\?.*state=error/);
  await expect.poll(() => mainWindowVisible(app)).toBe(true);
});
