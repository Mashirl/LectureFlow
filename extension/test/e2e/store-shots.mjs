// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Store screenshots (Chrome Web Store 1280 × 800, also used on AMO), the
// small promo tile (440 × 280) and the store icon (128 × 128, 96 × 96 art
// with a 16 px transparent margin), in English and Simplified Chinese:
//
//   npm run store-shots [-- --out DIR] [-- --lang en|zh-CN] [-- --no-app]
//
// writes extension/docs/store-assets/<version>/ by default. What is real
// and what is staged:
//
// - The extension is the e2e build of this version (scripts/build.mjs
//   --e2e: the store build plus host access to the demo site and the test
//   hooks), so every popup, card, row state and confirmation window is the
//   extension's own UI. The only change is in a throw-away copy: the
//   bundled profiles are replaced by a demo profile ("Demo platform" /
//   "演示平台": the Panopto profile's rules on the demo host), so that no
//   picture puts Panopto's name on a page that is not Panopto
//   (docs/extension/store/chrome-web-store.md §10).
// - The pages are a made-up recording platform and learning platform
//   (test/e2e/store-site.mjs) on lectures.example.edu and learn.example.edu,
//   served by a local server the browser uses as its proxy; nothing
//   reaches the internet.
// - LectureFlow is the e2e tests' fake behind the native port
//   (harness.mjs), with made-up courses. The main app's picture is the
//   mock server of the web front end (go run ./internal/api/mockserve,
//   scenario "downloads"), with its made-up data.
// - Each picture is composed in a plain browser: a caption band on top,
//   the page below at 1280 × 704 and, for the popup and the window, the
//   extension's own page rendered at twice the pixel density and laid over
//   the page where the browser would show it. No browser frame is drawn.
//
// Playwright's own Chromium with throw-away profiles, never the user's
// browser; the mock server is started and stopped by this script (its own
// process only).

import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import http from 'node:http';
import net from 'node:net';
import { spawn, execFileSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { installFakeLectureFlow } from './harness.mjs';
import { startStoreSite, HOST, LMS, SESSIONS } from './store-site.mjs';
import { checkProfile } from '../../src/lib/profile.js';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');
const repo = path.resolve(root, '..');
const pkg = JSON.parse(fs.readFileSync(path.join(root, 'package.json'), 'utf8'));
const argv = process.argv.slice(2);
const opt = (name, def) => (argv.includes(name) ? argv[argv.indexOf(name) + 1] : def);
const OUT = path.resolve(opt('--out', path.join(root, 'docs', 'store-assets', pkg.version)));
const LANGS = opt('--lang', '') ? [opt('--lang', '')] : ['en', 'zh-CN'];
const WITH_APP = !argv.includes('--no-app');

const W = 1280;
const H = 800;
const BAND = 96;
const PAGE_H = H - BAND;

const COURSE_IDS = ['01JSTORE000000000000000001', '01JSTORE000000000000000002'];
const COURSES = {
  en: [
    { id: COURSE_IDS[0], name: 'CSC1003 Cryptography' },
    { id: COURSE_IDS[1], name: 'CSC2011 Operating Systems' },
  ],
  'zh-CN': [
    { id: COURSE_IDS[0], name: 'CSC1003 密码学' },
    { id: COURSE_IDS[1], name: 'CSC2011 操作系统' },
  ],
};

// The captions: what each picture shows. Lectures are sent and fetched,
// never "downloaded" (user decision 2026-10-27).
const CAPTIONS = {
  en: {
    '01-popup': ['Send the lecture you are watching to LectureFlow', 'Pick what to send and the course: one click, one confirmation.'],
    '02-two-cameras': ['Two cameras? Send both', 'Lecture audio plus both screens; LectureFlow plays the second one picture-in-picture.'],
    '03-in-page': ['Or send it from the player page', 'On platforms with a profile, the button sits in the player’s own header.'],
    '04-whole-course': ['Update a whole course at once', 'Only lectures that are not in LectureFlow yet are sent; imported ones are skipped.'],
    '05-confirm-window': ['Embedded in a learning platform? Confirm in a separate window', 'The extension’s own window, so another website cannot trick you into clicking.'],
    '06-small-player': ['Small embedded player? The same window', 'On a website you trust, it opens without the warning.'],
    '07-in-lectureflow': ['LectureFlow fetches it, then transcribes and translates', 'Fetching runs in LectureFlow: close the tab any time, it goes on.'],
  },
  'zh-CN': {
    '01-popup': ['把正在看的录播发送至 LectureFlow', '选好内容和课程，点一下、确认一下。'],
    '02-two-cameras': ['双机位？一起发送', '主讲音频加两路画面；LectureFlow 以画中画播放第二路。'],
    '03-in-page': ['也可以直接在播放页发送', '在有对策文件的平台上，按钮就在播放页的标题栏里。'],
    '04-whole-course': ['一次更新整门课程', '只发送 LectureFlow 里还没有的课，已导入的自动跳过。'],
    '05-confirm-window': ['嵌在学习平台里？在单独的窗口里确认', '扩展自己的窗口，其他网站无法诱导你点击。'],
    '06-small-player': ['嵌入的播放器太小？同样在窗口里确认', '在你信任的网站里，窗口不再显示提醒。'],
    '07-in-lectureflow': ['LectureFlow 刮削后自动转写和翻译', '刮削在 LectureFlow 里进行，关掉标签页也不影响。'],
  },
};

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const b64 = (buf) => `data:image/png;base64,${buf.toString('base64')}`;
const esc = (s) => String(s).replace(/[&<>"]/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' })[c]);

if (!process.env.PLAYWRIGHT_BROWSERS_PATH) process.env.PLAYWRIGHT_BROWSERS_PATH = path.join(repo, 'tools', 'ms-playwright');
const { chromium } = await import('playwright');

// ------------------------------------------------------------ extension

/** The demo profile: the Panopto profile's rules on the demo host, under another name. */
function demoProfile(lang) {
  const p = JSON.parse(fs.readFileSync(path.join(root, 'profiles', 'panopto.json'), 'utf8'));
  p.id = 'demo';
  p.version = '1.0.0';
  p.name = lang === 'en' ? 'Demo platform' : '演示平台';
  p.match = { ...p.match, hosts: [HOST] };
  // the demo pages carry the date in a <time> element
  p.lecture.recordedAt = [{ selector: 'time#recorded', attr: 'datetime' }, ...p.lecture.recordedAt];
  p.fixtures = 'fixtures/demo/';
  const errors = checkProfile(JSON.parse(fs.readFileSync(path.join(root, 'schema', 'profile.schema.json'), 'utf8')), p);
  if (errors.length) throw new Error(`demo profile: ${errors.map((e) => `${e.path} ${e.message}`).join('; ')}`);
  return p;
}

/** A throw-away copy of dist/chrome-e2e with the demo profile only and access to the demo platform. */
function demoExtension(lang) {
  const src = path.join(root, 'dist', 'chrome-e2e');
  if (!fs.existsSync(path.join(src, 'manifest.json'))) throw new Error('run "node scripts/build.mjs --target chrome --e2e" first (npm run store-shots does)');
  const built = JSON.parse(fs.readFileSync(path.join(src, 'manifest.json'), 'utf8'));
  if (built.version !== pkg.version) throw new Error(`dist/chrome-e2e is ${built.version}, package.json says ${pkg.version}: rebuild`);
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'lf-store-ext-'));
  fs.cpSync(src, dir, { recursive: true });
  const profiles = path.join(dir, 'profiles');
  for (const f of fs.readdirSync(profiles)) if (f.endsWith('.json')) fs.rmSync(path.join(profiles, f));
  fs.writeFileSync(path.join(profiles, 'demo.json'), JSON.stringify(demoProfile(lang), null, 2) + '\n');
  fs.writeFileSync(path.join(profiles, 'index.json'), JSON.stringify({ profiles: ['demo.json'] }, null, 2) + '\n');
  const m = JSON.parse(fs.readFileSync(path.join(dir, 'manifest.json'), 'utf8'));
  m.host_permissions = [`http://${HOST}/*`];
  fs.writeFileSync(path.join(dir, 'manifest.json'), JSON.stringify(m, null, 2) + '\n');
  return dir;
}

async function startExtensionBrowser(lang, site) {
  const ext = demoExtension(lang);
  const userDataDir = fs.mkdtempSync(path.join(os.tmpdir(), 'lf-store-profile-'));
  const context = await chromium.launchPersistentContext(userDataDir, {
    channel: 'chromium',
    headless: true,
    viewport: { width: W, height: PAGE_H },
    deviceScaleFactor: 2,
    locale: lang,
    colorScheme: 'light',
    args: [
      `--disable-extensions-except=${ext}`,
      `--load-extension=${ext}`,
      `--lang=${lang}`,
      // every request goes to the demo site (it answers only its two hosts)
      `--proxy-server=http://127.0.0.1:${site.port}`,
      '--host-resolver-rules=MAP * ~NOTFOUND, EXCLUDE 127.0.0.1',
      '--disable-features=HttpsUpgrades,HttpsFirstBalancedModeAutoEnable',
    ],
  });
  const worker = context.serviceWorkers()[0] || (await context.waitForEvent('serviceworker'));
  const extensionId = new URL(worker.url()).host;
  for (let i = 0; i < 100; i++) {
    if ((await worker.evaluate(() => typeof globalThis.__lectureflowTest)) === 'object') break;
    await sleep(50);
  }
  await worker.evaluate(() => globalThis.__lectureflowTest.syncObserver());
  await worker.evaluate(
    ({ ui, course }) => chrome.storage.local.set({ uiLanguage: ui, lastCourseId: course }),
    { ui: lang === 'en' ? 'en' : 'zh_CN', course: COURSE_IDS[0] },
  );
  return {
    context,
    worker,
    extensionId,
    async close() {
      await context.close();
      fs.rmSync(userDataDir, { recursive: true, force: true });
      fs.rmSync(ext, { recursive: true, force: true });
    },
  };
}

const fakeLectureFlow = (worker, opts) => worker.evaluate(`(${installFakeLectureFlow})(${JSON.stringify(opts)}, globalThis)`);

async function tabIdOf(worker, page) {
  const url = page.url();
  return worker.evaluate(async (u) => {
    const tabs = await chrome.tabs.query({});
    const t = tabs.find((x) => x.url === u);
    return t ? t.id : null;
  }, url);
}

/** The page as it looks (pointer away, nothing focused), at device pixels. */
async function pageShot(page) {
  await page.mouse.move(2, PAGE_H - 2);
  await page.evaluate(() => document.activeElement && document.activeElement.blur && document.activeElement.blur());
  await sleep(250);
  return page.screenshot({ scale: 'device' });
}

/** The popup for a tab, after `act`; returns { png, w, h } (CSS pixels). */
async function popupShot(B, tabId, view, act) {
  const popup = await B.context.newPage();
  await popup.setViewportSize({ width: 360, height: 120 });
  await popup.goto(`chrome-extension://${B.extensionId}/popup/popup.html?tabId=${tabId}`);
  await popup.waitForSelector(`body[data-view="${view}"]`, { timeout: 15000 });
  if (act) await act(popup);
  await popup.mouse.move(1, 1);
  await sleep(300);
  const png = await popup.screenshot({ fullPage: true, scale: 'device' });
  const size = await popup.evaluate(() => ({ w: document.documentElement.scrollWidth, h: document.documentElement.scrollHeight }));
  await popup.close();
  return { png, ...size };
}

async function openPage(B, url, ready) {
  const page = await B.context.newPage();
  await page.goto(url);
  if (ready) await page.waitForSelector(ready, { timeout: 15000 });
  await sleep(500);
  return page;
}

/** The confirmation window a click opens, once its button is armed; returns { png, w, h }. */
async function windowShot(B, click) {
  const isConfirm = (p) => p.url().startsWith(`chrome-extension://${B.extensionId}/confirm/confirm.html`);
  const [win] = await Promise.all([B.context.waitForEvent('page', { predicate: isConfirm, timeout: 15000 }), click()]);
  await win.waitForLoadState();
  // the window is 420 × 680 outside (src/background/main.js); about 649 inside
  await win.setViewportSize({ width: 420, height: 649 });
  await win.waitForSelector('body[data-view="ready"]', { timeout: 15000 });
  await win.waitForSelector('#primary:not([disabled])', { timeout: 15000 });
  await sleep(900);
  await win.mouse.move(1, 1);
  const png = await win.screenshot({ fullPage: true, scale: 'device' });
  const size = await win.evaluate(() => ({ w: document.documentElement.scrollWidth, h: document.documentElement.scrollHeight }));
  await win.close();
  return { png, ...size };
}

async function extensionScenes(lang) {
  const site = await startStoreSite({ lang });
  const B = await startExtensionBrowser(lang, site);
  const scenes = {};
  const courses = COURSES[lang];
  const viewer = `http://${HOST}/Panopto/Pages/Viewer.aspx?id=${SESSIONS[0]}`;
  try {
    // 1. the popup, ready: "Main audio & video" is the full recording
    await fakeLectureFlow(B.worker, { courses, nextTaskId: 50 });
    let page = await openPage(B, viewer, 'body[data-ready="1"]');
    await page.waitForSelector('lectureflow-ui[data-kind="player"]', { timeout: 15000 });
    let bg = await pageShot(page);
    let pop = await popupShot(B, await tabIdOf(B.worker, page), 'ready', async (p) => {
      const pressed = await p.getAttribute('#presets [data-preset="main"]', 'aria-pressed');
      if (pressed !== 'true') throw new Error('popup: "main" is not chosen');
    });
    scenes['01-popup'] = { bg, overlay: { ...pop, kind: 'popup' } };

    // 3. the player's own button and its card (same page)
    await page.locator('lectureflow-ui[data-kind="player"] .ip-btn').click();
    await page.locator('lectureflow-ui[data-kind="card"] .lf-card[data-view="ready"]').waitFor({ timeout: 15000 });
    scenes['03-in-page'] = { bg: await pageShot(page) };
    await page.close();

    // 2. two screens: "Two cameras" chosen
    site.state.twoScreens = true;
    await fakeLectureFlow(B.worker, { courses, nextTaskId: 60 });
    page = await openPage(B, viewer, 'body[data-ready="1"]');
    await page.waitForSelector('lectureflow-ui[data-kind="player"]', { timeout: 15000 });
    bg = await pageShot(page);
    pop = await popupShot(B, await tabIdOf(B.worker, page), 'ready', async (p) => {
      await p.click('#presets [data-preset="dual"]');
      await p.waitForSelector('#presets [data-preset="dual"][aria-pressed="true"]');
    });
    scenes['02-two-cameras'] = { bg, overlay: { ...pop, kind: 'popup' } };
    await page.close();
    site.state.twoScreens = false;

    // 4. the folder list: row states, then "Update this course in LectureFlow"
    const [, , s6, s5, s4] = SESSIONS;
    await fakeLectureFlow(B.worker, {
      courses,
      checkMap: {
        [s6]: { state: 'downloading', taskId: 33 },
        [s5]: { state: 'imported', taskId: 5, lectureId: '01JSTORELECTURE00000000005' },
        [s4]: { state: 'imported', taskId: 4, lectureId: '01JSTORELECTURE00000000004' },
      },
      nextTaskId: 70,
    });
    page = await openPage(B, `http://${HOST}/Panopto/Pages/Sessions/List.aspx`, 'body[data-ready="1"]');
    await page.waitForSelector(`tr[id="${s5}"] [data-row-state="imported"]`, { timeout: 15000 });
    await B.worker.evaluate((sid) => globalThis.__fakeLF.push({ taskId: 33, platform: 'demo', sessionId: sid, state: 'downloading', bytesDone: 42, bytesTotal: 100 }), s6);
    await page.waitForSelector(`tr[id="${s6}"] [data-row-state="downloading"]`, { timeout: 15000 });
    await page.locator('lectureflow-ui[data-kind="header"] .ip-btn').click();
    await page.locator('lectureflow-ui[data-kind="card"] .lf-card[data-phase="idle"]').waitFor({ timeout: 15000 });
    scenes['04-whole-course'] = { bg: await pageShot(page) };
    await page.close();

    // 5. embedded in a learning platform the user has not trusted: the window
    await B.worker.evaluate(() => chrome.storage.local.remove('trustedEmbedders'));
    await fakeLectureFlow(B.worker, { courses, nextTaskId: 80 });
    page = await openPage(B, `http://${LMS}/course/recordings`);
    let frame = page.frameLocator('iframe[name="tool_content"]');
    await frame.locator('body[data-ready="1"]').waitFor({ timeout: 15000 });
    let btn = frame.locator('h1#title lectureflow-ui[data-kind="player"] .ip-btn');
    await btn.waitFor({ timeout: 15000 });
    bg = await pageShot(page);
    let win = await windowShot(B, () => btn.click());
    scenes['05-confirm-window'] = { bg, overlay: { ...win, kind: 'window' } };
    await page.close();

    // 6. a small player on a trusted site: the window, plain
    await B.worker.evaluate((o) => chrome.storage.local.set({ trustedEmbedders: { [o]: '2026-10-20T10:00:00.000Z' } }), `http://${LMS}`);
    await fakeLectureFlow(B.worker, { courses, nextTaskId: 90 });
    page = await openPage(B, `http://${LMS}/course/module`);
    frame = page.frameLocator('iframe[name="tool_content"]');
    await frame.locator('body[data-ready="1"]').waitFor({ timeout: 15000 });
    btn = frame.locator('h1#title lectureflow-ui[data-kind="player"] .ip-btn');
    await btn.waitFor({ timeout: 15000 });
    bg = await pageShot(page);
    win = await windowShot(B, () => btn.click());
    scenes['06-small-player'] = { bg, overlay: { ...win, kind: 'window' } };
    await page.close();
  } finally {
    await B.close();
    await site.close();
  }
  const strays = site.requests.filter((r) => ![HOST, LMS].includes(r.host));
  if (strays.length) console.log(`(requests to other hosts, answered 404 / refused: ${[...new Set(strays.map((r) => `${r.method} ${r.host}`))].join(', ')})`);
  return scenes;
}

// ------------------------------------------------------------ the app

function freePort() {
  return new Promise((resolve, reject) => {
    const s = net.createServer();
    s.once('error', reject);
    s.listen(0, '127.0.0.1', () => {
      const { port } = s.address();
      s.close(() => resolve(port));
    });
  });
}

function request(base, method, p, body) {
  return new Promise((resolve, reject) => {
    const url = new URL(`${base}${p}${p.includes('?') ? '&' : '?'}mock=downloads`);
    const data = body === undefined ? null : Buffer.from(JSON.stringify(body));
    const req = http.request(url, { method, headers: data ? { 'content-type': 'application/json', 'content-length': data.length } : {} }, (res) => {
      let text = '';
      res.on('data', (c) => (text += c));
      res.on('end', () => resolve({ status: res.statusCode, text }));
    });
    req.on('error', reject);
    if (data) req.write(data);
    req.end();
  });
}

/** Builds and starts the web front end's mock server (its data is made up); returns { base, stop }. */
async function startMock() {
  const bin = path.join(fs.mkdtempSync(path.join(os.tmpdir(), 'lf-store-mock-')), process.platform === 'win32' ? 'mockserve.exe' : 'mockserve');
  execFileSync('go', ['build', '-o', bin, './internal/api/mockserve'], { cwd: repo, stdio: 'inherit' });
  const port = await freePort();
  const proc = spawn(bin, ['-addr', `127.0.0.1:${port}`, '-root', repo], { cwd: repo, stdio: 'ignore' });
  const base = `http://127.0.0.1:${port}`;
  for (let i = 0; i < 200; i++) {
    if (proc.exitCode !== null) throw new Error(`mock server exited with ${proc.exitCode}`);
    try {
      if ((await request(base, 'GET', '/api/app')).status === 200) break;
    } catch {
      // not up yet
    }
    await sleep(250);
  }
  return {
    base,
    async stop() {
      proc.kill();
      await new Promise((r) => (proc.exitCode !== null ? r() : proc.once('exit', r)));
      fs.rmSync(path.dirname(bin), { recursive: true, force: true });
    },
  };
}

const CRYPTO = '01JC0000000000000000003631'; // the mock's CSC1003 Cryptography

async function appScene(mock, lang) {
  if ((await request(mock.base, 'PUT', '/api/settings/ui', { locale: lang, zh_display: '' })).status !== 200) throw new Error('mock: locale');
  // Keep the fetches that tell the story (fetching, queued, merging, creating
  // the lecture); take away the failed, cancelled and needs-refresh ones.
  await request(mock.base, 'GET', '/api/downloads');
  for (const id of [46, 43]) await request(mock.base, 'POST', `/api/downloads/${id}/cancel`);
  for (const id of [46, 47, 48, 43]) await request(mock.base, 'DELETE', `/api/downloads/${id}`);
  // course codes of our own make in the sidebar (the store pictures use CSC… codes)
  await request(mock.base, 'PATCH', '/api/courses/01JC0000000000000000002120', { code: 'CSC2011' });
  await request(mock.base, 'PATCH', '/api/courses/01JC0000000000000000003311', { code: 'CSC3014' });
  const browser = await chromium.launch({ headless: true });
  try {
    const ctx = await browser.newContext({ viewport: { width: W, height: PAGE_H }, deviceScaleFactor: 2, colorScheme: 'light', locale: lang });
    // the resources panel folded away (the app remembers that choice)
    await ctx.addInitScript(() => localStorage.setItem('lectureflow.inspector', 'collapsed'));
    const page = await ctx.newPage();
    await page.goto(`${mock.base}/?mock=downloads&quiet=1&theme=light#/course/${CRYPTO}`);
    // the mock server's own marker is not part of the app
    await page.addStyleTag({ content: '.toasts,.brand-mock{display:none!important}' });
    await page.waitForSelector('.dl-list .dl-row', { timeout: 20000 });
    await sleep(1200);
    const markers = await page.evaluate(() => (document.body.innerText.match(/⟦[^⟧]*⟧/g) || []).slice(0, 5));
    if (markers.length) throw new Error(`app page shows untranslated keys: ${markers.join(', ')}`);
    return { bg: await pageShot(page) };
  } finally {
    await browser.close();
  }
}

// ------------------------------------------------------------ composing

const MARK = (fg, bg) =>
  `<svg viewBox="0 0 32 32" aria-hidden="true"><rect width="32" height="32" rx="8" fill="${bg}"/><path d="M9 13v6M13 10v12M17 8v16M21 11v10M25 14v4" stroke="${fg}" stroke-width="2.4" stroke-linecap="round" fill="none"/></svg>`;
const ACCENT = '#1f6feb';
const DISPLAY = "'Segoe UI Variable Display', 'Segoe UI', system-ui, 'Microsoft YaHei UI', 'Microsoft YaHei', 'PingFang SC', sans-serif";

function sceneHtml(scene, [title, sub], lang) {
  let overlay = '';
  if (scene.overlay) {
    const o = scene.overlay;
    if (o.kind === 'popup') {
      // hangs from the top right, where the browser opens it under its toolbar
      const s = Math.min(1.6, (PAGE_H - 20) / o.h);
      overlay = `<img class="ov popup" src="${b64(o.png)}" style="width:${Math.round(o.w * s)}px;height:${Math.round(o.h * s)}px;right:28px;top:0">`;
    } else {
      // a window of its own, in front of the page (to the right, so that
      // the button that opened it stays in view)
      const s = Math.min(1.15, (PAGE_H - 40) / o.h);
      overlay = `<img class="ov window" src="${b64(o.png)}" style="width:${Math.round(o.w * s)}px;height:${Math.round(o.h * s)}px;right:40px;top:20px">`;
    }
  }
  return `<!doctype html><html lang="${lang}"><head><meta charset="utf-8"><style>
*{box-sizing:border-box}
html,body{margin:0;width:${W}px;height:${H}px;overflow:hidden;background:#fff}
.band{display:flex;align-items:center;gap:22px;height:${BAND}px;padding:0 36px;background:${ACCENT};color:#fff;font-family:${DISPLAY}}
.band svg{width:48px;height:48px;flex:none}
.band h1{margin:0;font-size:29px;font-weight:600;line-height:1.2;letter-spacing:0}
.band p{margin:4px 0 0;font-size:17px;line-height:1.3;color:rgb(255 255 255 / 0.9)}
.scene{position:relative;width:${W}px;height:${PAGE_H}px;overflow:hidden}
.scene > img.bg{display:block;width:${W}px;height:${PAGE_H}px}
.ov{position:absolute;display:block;border:1px solid #bcc2cc;background:#fff}
.ov.popup{border-top:0;border-radius:0 0 10px 10px;box-shadow:0 18px 40px -10px rgb(16 24 40 / 0.35),0 4px 12px -4px rgb(16 24 40 / 0.18)}
.ov.window{border-radius:10px;box-shadow:0 24px 60px -12px rgb(16 24 40 / 0.45),0 6px 16px -6px rgb(16 24 40 / 0.25)}
</style></head><body>
<header class="band">${MARK(ACCENT, '#fff')}<div><h1>${esc(title)}</h1><p>${esc(sub)}</p></div></header>
<div class="scene"><img class="bg" src="${b64(scene.bg)}">${overlay}</div>
</body></html>`;
}

function promoHtml() {
  return `<!doctype html><html><head><meta charset="utf-8"><style>
html,body{margin:0;width:440px;height:280px;overflow:hidden}
body{display:flex;flex-direction:column;justify-content:center;gap:14px;padding:0 36px;box-sizing:border-box;background:${ACCENT};color:#fff;font-family:${DISPLAY}}
svg{width:64px;height:64px}
h1{margin:0;font-size:31px;font-weight:650;line-height:1.15}
p{margin:0;font-size:17px;line-height:1.35;color:rgb(255 255 255 / 0.92)}
</style></head><body>${MARK(ACCENT, '#fff')}<h1>Send to LectureFlow</h1><p>Lecture recordings → LectureFlow,<br>for transcription and translation.</p></body></html>`;
}

function iconHtml() {
  // 96 × 96 art in the middle of 128 × 128, transparent around (Chrome Web Store "Supplying images")
  return `<!doctype html><html><head><meta charset="utf-8"><style>
html,body{margin:0;width:128px;height:128px;background:transparent}
svg{position:absolute;left:16px;top:16px;width:96px;height:96px}
</style></head><body>${MARK('#ffffff', ACCENT)}</body></html>`;
}

async function render(browser, html, size, file, { transparent = false } = {}) {
  const ctx = await browser.newContext({ viewport: size, deviceScaleFactor: 1 });
  const page = await ctx.newPage();
  await page.setContent(html, { waitUntil: 'load' });
  await page.evaluate(() => document.fonts.ready);
  await page.screenshot({ path: file, omitBackground: transparent });
  await ctx.close();
}

/** Width and height of a PNG file. */
function pngSize(file) {
  const b = fs.readFileSync(file);
  return [b.readUInt32BE(16), b.readUInt32BE(20)];
}

// ------------------------------------------------------------ main

fs.mkdirSync(OUT, { recursive: true });
const written = [];
let mock = null;
try {
  if (WITH_APP) mock = await startMock();
  for (const lang of LANGS) {
    console.log(`${lang}: the extension's scenes`);
    const scenes = await extensionScenes(lang);
    if (mock) {
      console.log(`${lang}: LectureFlow's course page (mock server)`);
      scenes['07-in-lectureflow'] = await appScene(mock, lang);
    }
    const composer = await chromium.launch({ headless: true });
    try {
      const dir = path.join(OUT, lang);
      fs.mkdirSync(dir, { recursive: true });
      for (const [name, scene] of Object.entries(scenes)) {
        const file = path.join(dir, `${name}.png`);
        await render(composer, sceneHtml(scene, CAPTIONS[lang][name], lang), { width: W, height: H }, file);
        written.push(file);
      }
    } finally {
      await composer.close();
    }
  }
  const composer = await chromium.launch({ headless: true });
  try {
    await render(composer, promoHtml(), { width: 440, height: 280 }, path.join(OUT, 'promo-small-440x280.png'));
    await render(composer, iconHtml(), { width: 128, height: 128 }, path.join(OUT, 'store-icon-128.png'), { transparent: true });
    written.push(path.join(OUT, 'promo-small-440x280.png'), path.join(OUT, 'store-icon-128.png'));
  } finally {
    await composer.close();
  }
} finally {
  if (mock) await mock.stop();
}
for (const f of written) console.log(`${path.relative(root, f)}  ${pngSize(f).join(' × ')}`);
