// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Lifecycle cases (SPEC-v3 V1.1) that need a service which can be told to
// fail, crash, ask for a restart or report running tasks: fake service only.
'use strict';

const { spawn } = require('node:child_process');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { test, expect, waitForApp, request, fakeState, mainWindowVisible, DESKTOP } = require('./fixtures');

const ELECTRON = require(path.join(DESKTOP, 'node_modules', 'electron'));
const ERROR_PAGE = /splash\.html\?.*state=error/;

// The error page can replace the start screen within milliseconds; poll the
// URL instead of waiting for a load event that may belong to the aborted page.
async function waitForErrorPage(page) {
  await expect.poll(() => page.url(), { timeout: 30_000 }).toMatch(ERROR_PAGE);
  await expect(page.locator('#retry')).toBeVisible();
}

// pid of the fake service the page is talking to (null on the start/error page).
async function pagePid(page) {
  try {
    return await page.evaluate(async () => (await (await fetch('/test/state')).json()).pid);
  } catch {
    return null;
  }
}

function alive(pid) {
  try {
    process.kill(pid, 0);
    return true;
  } catch {
    return false;
  }
}

async function setFake(port, body) {
  const r = await request(port, '/test/set', { method: 'POST', body, headers: { 'Content-Type': 'application/json' } });
  expect(r.status).toBe(200);
}

function waitExit(app, ms = 20_000) {
  const proc = app.process();
  if (proc.exitCode !== null) return Promise.resolve(proc.exitCode);
  return new Promise((resolve, reject) => {
    const t = setTimeout(() => reject(new Error('app did not exit')), ms);
    proc.once('exit', (code) => {
      clearTimeout(t);
      resolve(code);
    });
  });
}

test('a start failure shows the reason, Open logs and Try again (zh-CN); Try again recovers', async ({ launch }) => {
  const { app, page, dataDir } = await launch({ env: { FAKE_START_FAIL_TIMES: '1' }, lang: 'zh-CN' });
  await waitForErrorPage(page);
  await expect.poll(() => mainWindowVisible(app)).toBe(true);
  await expect(page.locator('#title')).toHaveText('LectureFlow 没能启动');
  await expect(page.locator('#detail')).toHaveText('open database: database is locked');
  await expect(page.locator('#retry')).toHaveText('重试');
  await expect(page.locator('#logs')).toHaveText('查看日志');
  expect(await page.evaluate(() => [typeof window.lectureflow, Object.keys(window.lectureflowShell).sort().join(',')])).toEqual(['undefined', 'openLogs,restoreDefault,retry']);

  await app.evaluate(({ shell }) => {
    globalThis.__paths = [];
    shell.openPath = async (p) => {
      globalThis.__paths.push(p);
      return '';
    };
  });
  await page.click('#logs');
  await expect.poll(() => app.evaluate(() => globalThis.__paths)).toEqual([path.join(dataDir, 'logs')]);

  await page.click('#retry');
  await waitForApp(page);
  expect(fakeState(dataDir).attempts).toBe(2);
});

test('a taken port is retried with another port', async ({ launch }) => {
  const { page, dataDir } = await launch({ env: { FAKE_PORT_FAIL_TIMES: '2' } });
  const url = await waitForApp(page);
  const st = fakeState(dataDir);
  expect(st.attempts).toBe(3);
  expect(st.args[2]).toContain(`127.0.0.1:${url.port}`);
  expect(st.args[2]).not.toContain('-location-file'); // data dir from LECTUREFLOW_DATA_DIR
});

test('exit code 3: the service is started again and the window reloads', async ({ launch }) => {
  const { page, dataDir } = await launch();
  const first = await waitForApp(page);
  const pid1 = fakeState(dataDir).pids[0];
  expect(await pagePid(page)).toBe(pid1);
  await request(first.port, '/test/exit?code=3');
  await expect.poll(() => fakeState(dataDir).pids.length, { timeout: 30_000 }).toBe(2);
  const pid2 = fakeState(dataDir).pids[1];
  expect(pid2).not.toBe(pid1);
  await expect.poll(() => pagePid(page), { timeout: 30_000 }).toBe(pid2);
  expect(await page.evaluate(() => window.__fakeLoaded === true)).toBe(true);
  expect(await page.evaluate(async () => (await fetch('/api/health')).status)).toBe(200);
});

test('an unexpected stop shows the error page; Try again starts the service again', async ({ launch }) => {
  const { app, page, dataDir } = await launch();
  const { port } = await waitForApp(page);
  await request(port, '/test/exit?code=1');
  await waitForErrorPage(page);
  await expect(page.locator('#title')).toHaveText('The background service stopped unexpectedly');
  await expect(page.locator('#detail')).toHaveText('simulated failure for the test');
  expect(await mainWindowVisible(app)).toBe(true);
  await page.click('#retry');
  await waitForApp(page);
  expect(fakeState(dataDir).attempts).toBe(2);
});

test('closing with running tasks asks; "continue in background" + remember hides the window; a second launch shows it again', async ({ launch }) => {
  const { app, page, dataDir, env } = await launch();
  const { port } = await waitForApp(page);
  await setFake(port, { activity: { running: 1, transcribing: true }, settings: { close_behavior: 'ask' } });
  await app.evaluate(({ dialog, Notification }) => {
    globalThis.__dialogs = [];
    globalThis.__notes = [];
    dialog.showMessageBox = async (win, opts) => {
      globalThis.__dialogs.push(opts);
      return { response: 0, checkboxChecked: true };
    };
    Notification.prototype.show = function show() {
      globalThis.__notes.push([this.title, this.body]);
    };
  });
  await app.evaluate(({ BrowserWindow }) => BrowserWindow.getAllWindows()[0].close());
  await expect.poll(() => mainWindowVisible(app)).toBe(false);
  const dialogs = await app.evaluate(() => globalThis.__dialogs);
  expect(dialogs).toHaveLength(1);
  expect(dialogs[0].buttons).toEqual(['Continue in background', 'Quit and stop tasks', 'Cancel']);
  expect(dialogs[0].checkboxLabel).toBe('Remember my choice (you can change it in Settings)');
  expect(fakeState(dataDir).patches).toEqual([{ close_behavior: 'background' }]);
  await expect.poll(() => app.evaluate(() => globalThis.__notes)).toEqual([
    ['LectureFlow is working in the background', process.platform === 'darwin' ? 'Open it again from the Dock.' : 'Open it again from the icon in the notification area.'],
  ]);
  const pid = fakeState(dataDir).pids[0];
  expect(alive(pid)).toBe(true);

  // Single instance: a second start only brings the first window back.
  const second = spawn(ELECTRON, [DESKTOP], { env, stdio: 'ignore' });
  const code = await new Promise((resolve) => second.once('exit', resolve));
  expect(code).toBe(0);
  await expect.poll(() => mainWindowVisible(app)).toBe(true);
  expect(fakeState(dataDir).attempts).toBe(1);

  // Remembered: the next close goes to the background without asking.
  await app.evaluate(({ BrowserWindow }) => BrowserWindow.getAllWindows()[0].close());
  await expect.poll(() => mainWindowVisible(app)).toBe(false);
  expect(await app.evaluate(() => globalThis.__dialogs.length)).toBe(1);
  expect(await app.evaluate(() => globalThis.__notes.length)).toBe(1); // the hint is shown once per session

  const log = fs.readFileSync(path.join(dataDir, 'logs', 'desktop.log'), 'utf8');
  expect(log).toMatch(/close requested: ask \(running 1\)/);
  expect(log).toMatch(/close requested: background \(running 1\)/);
  expect(log).not.toMatch(/ERROR|unhandled/);
});

test('closing without running tasks quits and stops the service gracefully', async ({ launch }) => {
  const { app, page, dataDir } = await launch();
  await waitForApp(page);
  const pid = fakeState(dataDir).pids[0];
  const exited = waitExit(app);
  await app.evaluate(({ BrowserWindow }) => BrowserWindow.getAllWindows()[0].close()).catch(() => {});
  await exited;
  const st = fakeState(dataDir);
  expect(st.shutdownCalls).toBe(1);
  await expect.poll(() => alive(pid)).toBe(false);
});

test('close_behavior "quit" quits even while tasks run', async ({ launch }) => {
  const { app, page, dataDir } = await launch();
  const { port } = await waitForApp(page);
  await setFake(port, { activity: { running: 2 }, settings: { close_behavior: 'quit' } });
  const pid = fakeState(dataDir).pids[0];
  const exited = waitExit(app);
  await app.evaluate(({ BrowserWindow }) => BrowserWindow.getAllWindows()[0].close()).catch(() => {});
  await exited;
  await expect.poll(() => alive(pid)).toBe(false);
});

test('system sleep is blocked while tasks run and allowed again afterwards', async ({ launch }) => {
  const { app, page } = await launch();
  const { port } = await waitForApp(page);
  await app.evaluate(({ powerSaveBlocker }) => {
    globalThis.__psb = [];
    const started = new Set();
    let next = 100;
    powerSaveBlocker.start = (type) => {
      globalThis.__psb.push(['start', type]);
      started.add(++next);
      return next;
    };
    powerSaveBlocker.stop = (id) => {
      globalThis.__psb.push(['stop', id]);
      started.delete(id);
    };
    powerSaveBlocker.isStarted = (id) => started.has(id);
  });
  await setFake(port, { activity: { running: 1 }, settings: { prevent_sleep: true } });
  await expect.poll(() => app.evaluate(() => globalThis.__psb), { timeout: 15_000 }).toEqual([['start', 'prevent-app-suspension']]);
  await setFake(port, { activity: { running: 0 } });
  await expect.poll(() => app.evaluate(() => globalThis.__psb), { timeout: 15_000 }).toEqual([['start', 'prevent-app-suspension'], ['stop', 101]]);
});

test("the shell follows the app's language choice and remembers it for the next start", async ({ launch }) => {
  const { app, page } = await launch({ lang: 'en-US' });
  await waitForApp(page);
  const port = new URL(page.url()).port;
  const memo = path.join(await app.evaluate(({ app: a }) => a.getPath('userData')), 'locale.json');
  const read = () => {
    try {
      return JSON.parse(fs.readFileSync(memo, 'utf8')).locale;
    } catch {
      return null;
    }
  };
  expect(read()).toBe(null);
  await setFake(port, { ui: { locale: 'ru' } });
  await expect.poll(read, { timeout: 15_000 }).toBe('ru');
  await setFake(port, { ui: { locale: 'yue-Hant-HK' } });
  await expect.poll(read, { timeout: 15_000 }).toBe('yue-Hant-HK');
  await setFake(port, { ui: { locale: '' } });
  await expect.poll(read, { timeout: 15_000 }).toBe(null);
});

test('no menu bar on Windows; Ctrl + = / Ctrl + 0 still zoom', async ({ launch }) => {
  const { app, page } = await launch();
  await waitForApp(page);
  const press = (keyCode) => app.evaluate(({ BrowserWindow }, k) => {
    const wc = BrowserWindow.getAllWindows()[0].webContents;
    wc.sendInputEvent({ type: 'keyDown', keyCode: k, modifiers: ['control'] });
    wc.sendInputEvent({ type: 'keyUp', keyCode: k, modifiers: ['control'] });
  }, keyCode);
  const zoom = () => app.evaluate(({ BrowserWindow }) => BrowserWindow.getAllWindows()[0].webContents.getZoomLevel());
  if (process.platform !== 'darwin') {
    expect(await app.evaluate(({ BrowserWindow, Menu }) => [Menu.getApplicationMenu(), BrowserWindow.getAllWindows()[0].isMenuBarVisible()])).toEqual([null, false]);
  }
  await press('=');
  await expect.poll(zoom).toBe(0.5);
  await press('0');
  await expect.poll(zoom).toBe(0);
});

test('an unreadable location.json: "use the default location" renames it and starts with the default folder', async ({ launch }) => {
  const appData = fs.mkdtempSync(path.join(os.tmpdir(), 'lf-appdata-'));
  try {
    const root = path.join(appData, 'LectureFlow');
    fs.mkdirSync(root, { recursive: true });
    fs.writeFileSync(path.join(root, 'location.json'), '{"data_dir": "relative/path"}');
    const { app, page } = await launch({ appData });
    await waitForErrorPage(page);
    await expect(page.locator('#title')).toHaveText('The data folder setting cannot be read');
    await expect(page.locator('#restore')).toHaveText('Use the default location…');
    await app.evaluate(({ dialog }) => {
      globalThis.__dialogs = [];
      dialog.showMessageBox = async (win, opts) => {
        globalThis.__dialogs.push(opts);
        return { response: globalThis.__dialogs.length === 1 ? 1 : 0 }; // first cancel, then confirm
      };
    });
    await page.click('#restore');
    await expect.poll(() => app.evaluate(() => globalThis.__dialogs.length)).toBe(1);
    expect(fs.existsSync(path.join(root, 'location.json'))).toBe(true); // cancelled: nothing changed
    await page.click('#restore');
    await waitForApp(page);
    const dialogs = await app.evaluate(() => globalThis.__dialogs);
    expect(dialogs[1].message).toBe('Use the default data folder?');
    expect(dialogs[1].detail).toContain(root);
    expect(fs.existsSync(path.join(root, 'location.json'))).toBe(false);
    expect(fs.readFileSync(path.join(root, 'location.json.bak'), 'utf8')).toBe('{"data_dir": "relative/path"}');
    const st = fakeState(root); // the service runs on the default folder
    expect(st.args[0]).toEqual(expect.arrayContaining(['-data-dir', root, '-location-file', path.join(root, 'location.json')]));
    await app.close();
  } finally {
    fs.rmSync(appData, { recursive: true, force: true, maxRetries: 10, retryDelay: 300 });
  }
});

test('"use the default location" is not offered for other errors', async ({ launch }) => {
  const { page } = await launch({ env: { FAKE_START_FAIL_TIMES: '1' } });
  await waitForErrorPage(page);
  await expect(page.locator('#restore')).toBeHidden();
});
