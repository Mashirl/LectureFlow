// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// What the popup and in-page end-to-end tests share: Playwright's own
// Chromium (tools/ms-playwright, a throw-away profile, never the user's
// browser) with dist/chrome-e2e loaded and the test host names mapped to
// the local test server (every other name fails to resolve, so nothing
// reaches the internet); a fake LectureFlow behind the native port,
// installed through the e2e build's test hook (no registry, no native
// host); recorders of cookies.getAll; screenshots on request.

import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { beforeEach } from 'node:test';
import { fileURLToPath } from 'node:url';
import { startServer } from './server.mjs';

export const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');
const repo = path.resolve(root, '..');
const extDir = path.join(root, 'dist', 'chrome-e2e');
export const shotsDir = path.join(root, 'docs', 'screenshots');
export const SHOTS = process.env.LF_EXT_SCREENSHOTS === '1';
if (!process.env.PLAYWRIGHT_BROWSERS_PATH) process.env.PLAYWRIGHT_BROWSERS_PATH = path.join(repo, 'tools', 'ms-playwright');

// Made-up values: they must show up only where a test says.
export const COOKIE_TRACK = 'LF-E2E-TRACK-COOKIE-7f3a9c';
export const COOKIE_OTHER = 'LF-E2E-OTHER-COOKIE-2b8e41';
export const COURSES = [
  { id: '01JE2E00000000000000000001', name: 'CSC1003 Data Structures' },
  { id: '01JE2E00000000000000000002', name: 'CSC2011 Operating Systems' },
];

/** Starts the test server and the browser; returns the handles and a close(). */
export async function startBrowser({ viewport = null, lang = 'zh-CN' } = {}) {
  assert.ok(fs.existsSync(path.join(extDir, 'manifest.json')), 'run "node scripts/build.mjs --target chrome --e2e" first');
  const { chromium } = await import('playwright');
  const server = await startServer();
  const userDataDir = fs.mkdtempSync(path.join(os.tmpdir(), 'lf-ext-e2e-'));
  const context = await chromium.launchPersistentContext(userDataDir, {
    channel: 'chromium',
    headless: true,
    viewport,
    args: [
      `--disable-extensions-except=${extDir}`,
      `--load-extension=${extDir}`,
      `--lang=${lang}`,
      // test host names → the local server (*.lf-e2e-x.test: a site the
      // extension has no access to; four do-not-handle sites for
      // blocklist.test.mjs, served by the local server, never the real
      // ones); everything else fails to resolve
      '--host-resolver-rules=MAP *.lf-e2e.test 127.0.0.1, MAP *.lf-e2e-x.test 127.0.0.1, MAP *.cloud.panopto.eu 127.0.0.1, MAP *.youtube-nocookie.com 127.0.0.1, MAP *.udemy.com 127.0.0.1, MAP *.edx.org 127.0.0.1, MAP *.linkedin.com 127.0.0.1, MAP * ~NOTFOUND, EXCLUDE 127.0.0.1',
    ],
  });
  const worker = context.serviceWorkers()[0] || (await context.waitForEvent('serviceworker'));
  const extensionId = new URL(worker.url()).host;
  for (let i = 0; i < 100; i++) {
    if ((await worker.evaluate(() => typeof globalThis.__lectureflowTest)) === 'object') break;
    await new Promise((r) => setTimeout(r, 50));
  }
  // The content scripts are registered on install; make sure they are.
  await worker.evaluate(() => globalThis.__lectureflowTest.syncObserver());
  // Record every cookies.getAll call from now on: at the browser API and at
  // the extension's one place that reads cookies.
  await worker.evaluate(() => {
    globalThis.__cookieCalls = globalThis.__lectureflowTest.recordCookieCalls();
    globalThis.__rawCookieCalls = [];
    const real = chrome.cookies.getAll;
    chrome.cookies.getAll = function (...args) {
      globalThis.__rawCookieCalls.push(JSON.parse(JSON.stringify(args[0])));
      return real.apply(this, args);
    };
  });
  await context.addCookies([
    { name: 'lf_track', value: COOKIE_TRACK, domain: 'course.lf-e2e.test', path: '/' },
    { name: 'lf_other', value: COOKIE_OTHER, domain: 'other.lf-e2e.test', path: '/' },
  ]);
  if (SHOTS) fs.mkdirSync(shotsDir, { recursive: true });
  const initialPages = new Set(context.pages());
  return {
    server,
    context,
    worker,
    extensionId,
    /**
     * fresh(): nothing an earlier test left behind reaches this one (see
     * isolateTests). Closes the pages opened since the start that are still
     * open (a test that failed did not get to close them); after such a
     * close, waits until the fake LectureFlow has had no new message for
     * 300 ms (at most about 5 s: the background may still be answering
     * those pages); then empties the fake's records.
     */
    async fresh() {
      const left = context.pages().filter((p) => !initialPages.has(p) && !p.isClosed());
      for (const p of left) await p.close().catch(() => {});
      const count = () => worker.evaluate(() => (globalThis.__fakeLF ? globalThis.__fakeLF.received.length : 0));
      if (left.length) {
        let last = await count();
        for (let i = 0; i < 16; i++) {
          await new Promise((r) => setTimeout(r, 300));
          const n = await count();
          if (n === last) break;
          last = n;
        }
      }
      await worker.evaluate(() => {
        const fake = globalThis.__fakeLF;
        if (fake) {
          fake.received.length = 0;
          fake.launches = 0;
        }
      });
    },
    async close() {
      await context.close();
      await server.close();
      // Chromium's last processes may still hold files of the profile for a
      // moment after the close (EPERM on a loaded machine)
      fs.rmSync(userDataDir, { recursive: true, force: true, maxRetries: 10, retryDelay: 300 });
    },
  };
}

/**
 * isolateTests(() => B): before each test of the file, B.fresh(). Under
 * load one test's wait timing out used to fail the ones after it: its
 * pages stayed open, kept the background busy and kept talking to the fake
 * LectureFlow, whose records the next test then read as its own (the
 * noLaunch check in inpage.test.mjs counted an earlier page's hello;
 * flake-hunt #13). Call it at the top level of a test file whose before()
 * sets B = await startBrowser(). A browser that does not answer within two
 * minutes fails the test instead of holding up the run.
 */
export function isolateTests(getBrowser) {
  beforeEach(async () => {
    const b = getBrowser();
    if (b) await b.fresh();
  }, { timeout: 120_000 });
}

/**
 * A fake LectureFlow behind the native port (runs in the service worker).
 * opts: mode (ok | absent | not_running | stopped), courses, checkItem (for every
 * session), checkMap (per session), titles ({ courseId: [titles] } a
 * lecture of that course already has — the dedup by title), nextTaskId,
 * position. `stopped` is LectureFlow not running behind a real host: a
 * hello with noLaunch gets app.not_running {reason: not_running}; any
 * other hello "starts" it (mode becomes ok, `launches` counts).
 */
export async function fakeLectureFlow(worker, opts = {}) {
  await worker.evaluate(`(${installFakeLectureFlow})(${JSON.stringify({ courses: COURSES, ...opts })}, globalThis)`);
}

/**
 * The fake itself, run in the extension's background: `g` is the
 * background's global (the service worker's own on Chromium; on Firefox
 * the background page as an extension page reaches it, test/firefox).
 * Only data crosses in `o`.
 */
export function installFakeLectureFlow(o, g) {
  {
    const norm = (s) => String(s || '').toLowerCase().replace(/[\s/\\:_-]+/g, ' ').trim();
    const fake = (g.__fakeLF = {
      mode: o.mode || 'ok',
      courses: o.courses || [],
      checkItem: o.checkItem || null,
      checkMap: o.checkMap || {},
      titles: Object.fromEntries(Object.entries(o.titles || {}).map(([k, v]) => [k, v.map(norm)])),
      nextTaskId: o.nextTaskId || 41,
      position: o.position ?? 2,
      launches: 0,
      received: [],
      ports: new Set(),
      push(status) {
        for (const p of fake.ports) p._emit({ type: 'status', platform: '', sessionId: '', courseId: '', position: 0, bytesDone: 0, bytesTotal: -1, ...status });
      },
    });
    g.__lectureflowTest.setNativeConnect(() => {
      if (fake.mode === 'absent') throw new Error('Specified native messaging host not found.');
      const onMsg = [];
      let open = true;
      const port = {
        onMessage: { addListener: (f) => onMsg.push(f) },
        onDisconnect: { addListener: () => {} },
        postMessage(m) {
          if (!open) throw new Error('closed');
          const copy = JSON.parse(JSON.stringify(m));
          fake.received.push(copy);
          g.setTimeout(() => answer(copy), 10);
        },
        disconnect() {
          open = false;
          fake.ports.delete(port);
        },
        _emit(m) {
          if (open) for (const f of onMsg) f(JSON.parse(JSON.stringify(m)));
        },
      };
      const reply = (m, body) => port._emit({ type: m.type, id: m.id, ok: true, ...body });
      function check(m) {
        const items = [];
        m.sessionIds.forEach((sessionId, i) => {
          const it = fake.checkMap[sessionId] || fake.checkItem;
          if (it) {
            items.push({ sessionId, matchedBy: 'session', ...it });
            return;
          }
          const title = m.titles ? m.titles[i] : '';
          if (title && (m.courseId ? fake.titles[m.courseId] || [] : Object.values(fake.titles).flat()).includes(norm(title))) {
            items.push({ sessionId, state: 'imported', lectureId: `01JE2ETITLE${i}`, matchedBy: 'title' });
          }
        });
        return items;
      }
      function answer(m) {
        if (fake.mode === 'stopped') {
          if (m.type === 'hello' && m.noLaunch === true) return port._emit({ type: m.type, id: m.id, ok: false, error: { code: 'app.not_running', params: { reason: 'not_running' } } });
          fake.mode = 'ok';
          fake.launches++;
        }
        if (fake.mode === 'not_running') return port._emit({ type: m.type, id: m.id, ok: false, error: { code: 'app.not_running', params: { reason: 'start_failed' } } });
        switch (m.type) {
          case 'hello':
            return reply(m, { protocol: 1, app: { version: '3.2.0', running: true, mode: 'desktop' }, courses: fake.courses });
          case 'check':
            return reply(m, { items: check(m) });
          case 'enqueue':
            return reply(m, { taskId: fake.nextTaskId++, position: fake.position });
          case 'refresh':
            return reply(m, { taskId: fake.checkItem ? fake.checkItem.taskId : 0, state: 'queued' });
          case 'open':
            return reply(m, {});
          default:
            return port._emit({ type: m.type, id: m.id, ok: false, error: { code: 'request.unknown_type', params: { type: m.type } } });
        }
      }
      fake.ports.add(port);
      return port;
    });
  }
}

export const received = (worker) => worker.evaluate(() => globalThis.__fakeLF.received.filter((m) => m.type !== 'hello'));
export const hellos = (worker) => worker.evaluate(() => globalThis.__fakeLF.received.filter((m) => m.type === 'hello'));
export const launches = (worker) => worker.evaluate(() => globalThis.__fakeLF.launches);
export const cookieCalls = (worker) => worker.evaluate(() => ({ module: [...globalThis.__cookieCalls], raw: [...globalThis.__rawCookieCalls] }));
export const resetCookieCalls = (worker) =>
  worker.evaluate(() => {
    globalThis.__cookieCalls.length = 0;
    globalThis.__rawCookieCalls.length = 0;
  });
/** Everything the extension keeps: both storage areas, as JSON. */
export const storageDump = (worker) => worker.evaluate(async () => JSON.stringify([await chrome.storage.local.get(null), await chrome.storage.session.get(null)]));

export async function tabIdOf(worker, page) {
  const url = page.url();
  return worker.evaluate(async (u) => {
    const tabs = await chrome.tabs.query({});
    const t = tabs.find((x) => x.url === u);
    return t ? t.id : null;
  }, url);
}

/** Light and dark screenshots of a page or element (LF_EXT_SCREENSHOTS=1). */
export async function shoot(page, name, target = null, opts = {}) {
  if (!SHOTS) return;
  for (const scheme of ['light', 'dark']) {
    await page.emulateMedia({ colorScheme: scheme });
    await page.waitForTimeout(150);
    const file = path.join(shotsDir, `${name}-${scheme}.png`);
    if (target) await target.screenshot({ path: file, ...opts });
    else await page.screenshot({ path: file, fullPage: true, ...opts });
  }
  await page.emulateMedia({ colorScheme: 'light' });
}
