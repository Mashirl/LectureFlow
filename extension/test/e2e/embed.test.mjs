// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// End to end, the in-page buttons inside other sites' pages (security
// review M4, lead ruling 2026-10-27): a page the extension has no access to
// (*.lf-e2e-x.test) embeds the Panopto player or list — in a transparent
// iframe, as a clickjacking page would. The button opens the extension's
// own window (the top-level site from location.ancestorOrigins), which
// confirms after 600 ms and offers to trust the site; the frame itself can
// detect and send nothing. On a trusted site the card stays in the page,
// armed after 600 ms, and a card that is not really visible (Chrome's
// IntersectionObserver v2) hands the confirmation to the window, and so
// does a small embedded player with no room for the card (without the
// click-jacking note when the site is trusted). The bulk "更新这门课程" goes
// the same way; the settings list the trusted sites.

import { test, before, after } from 'node:test';
import assert from 'node:assert/strict';
import * as H from './harness.mjs';
import { PANOPTO, LIST } from './server.mjs';

const { COURSES } = H;
let B;
let server;
let context;
let worker;
let extensionId;

before(async () => {
  B = await H.startBrowser({ viewport: { width: 1280, height: 800 } });
  ({ server, context, worker, extensionId } = B);
});

// a test that failed leaves nothing behind for the next one (harness.mjs)
H.isolateTests(() => B);

after(async () => {
  if (B) await B.close();
});

const port = () => server.port;
const ATTACKER = () => `http://attacker.lf-e2e-x.test:${port()}`;
const CANVAS = () => `http://canvas.lf-e2e.test:${port()}`;
const received = () => H.received(worker);
const enqueues = async () => (await received()).filter((m) => m.type === 'enqueue');
const S = (n) => LIST[n - 1].id;
const listIds = new Set(LIST.map((s) => s.id));
const listInfoRequests = () => server.requests.filter((r) => r.path.endsWith('/DeliveryInfo.aspx') && listIds.has(r.deliveryId));
const isConfirm = (p) => p.url().startsWith(`chrome-extension://${extensionId}/confirm/confirm.html`);
const trusted = () => worker.evaluate(async () => (await chrome.storage.local.get('trustedEmbedders')).trustedEmbedders || {});
const setTrusted = (origins) => worker.evaluate((list) => (list.length ? chrome.storage.local.set({ trustedEmbedders: Object.fromEntries(list.map((o) => [o, new Date().toISOString()])) }) : chrome.storage.local.remove('trustedEmbedders')), origins);
const ticket = () => worker.evaluate(() => {
  const t = [...globalThis.__lectureflowTest.tickets.values()].pop();
  return t ? { tabId: t.tabId, frameId: t.frameId, frameUrl: t.frameUrl, site: t.site, flow: t.flow } : null;
});
const frameCommand = (t, msg) => worker.evaluate(({ t, msg }) => globalThis.__lectureflowTest.inpageCommand(t.tabId, t.frameId, t.frameUrl, msg), { t, msg });

async function open(url, ready) {
  const page = await context.newPage();
  await page.goto(url);
  if (ready) await page.waitForSelector(ready, { timeout: 10000 });
  return page;
}

/** Clicks and returns the confirmation window the click opens. */
async function windowFrom(click) {
  const [win] = await Promise.all([context.waitForEvent('page', { predicate: isConfirm, timeout: 15000 }), click()]);
  await win.waitForLoadState();
  return win;
}

/** A confirming control acts once it has been shown (enabled) for 600 ms. */
async function clickArmed(locator) {
  await locator.waitFor({ state: 'visible', timeout: 15000 });
  for (let i = 0; i < 150 && !(await locator.isEnabled()); i++) await new Promise((r) => setTimeout(r, 100));
  await new Promise((r) => setTimeout(r, 700));
  await locator.click();
}

test('a page the extension cannot see embeds the player in a transparent iframe: the button opens the extension’s window, the frame itself sends nothing', async () => {
  server.panopto.downloadUrl = '/Panopto/Podcast/Download/whole.mp4';
  await setTrusted([]);
  await H.fakeLectureFlow(worker, { nextTaskId: 400 });
  const page = await open(`${ATTACKER()}/canvas/embed.html?hidden=1`);
  const frame = page.frameLocator('iframe[name="tool_content"]');
  await frame.locator('body[data-ready="1"]').waitFor({ timeout: 10000 });
  const btn = frame.locator('h1#title lectureflow-ui[data-kind="player"] .ip-btn');
  await btn.waitFor({ timeout: 10000 });
  const win = await windowFrom(() => btn.click());
  // The window: a click the moment its button is ready does nothing.
  await win.waitForSelector('#primary:not([disabled])', { timeout: 15000 });
  await win.evaluate(() => document.getElementById('primary').click());
  assert.equal(await frame.locator('lectureflow-ui[data-kind="card"]').count(), 0, 'no card in the page');
  assert.equal(await win.evaluate(() => document.body.dataset.view), 'ready');
  assert.match(await win.textContent('#embed-why'), new RegExp(`嵌在 ${ATTACKER().replace(/[.:/]/g, '\\$&')} 的网页里`));
  assert.match(await win.textContent('#embed-from'), new RegExp(`录播页面：${PANOPTO.host.replace(/\./g, '\\.')}:\\d+`));
  assert.equal(await win.isVisible('#trust-line'), true);
  assert.equal(await win.isChecked('#trust'), false);
  assert.match(await win.textContent('#trust-line'), /以后在 http:\/\/attacker\.lf-e2e-x\.test:\d+ 里直接在页面上确认/);
  assert.match(await win.textContent('#lecture'), /CSC1003\/L01\/01/);
  assert.match(await win.textContent('#primary'), /确认发送/);
  await H.shoot(win, 'confirm-window');
  await win.waitForTimeout(300);
  assert.deepEqual(await enqueues(), [], 'the early click was dropped');

  // The background: the frame's own commands are refused; it knows the top site from ancestorOrigins.
  const t = await ticket();
  assert.equal(t.site, ATTACKER());
  assert.equal(t.flow, 'single');
  assert.equal((await frameCommand(t, { cmd: 'send', trackIds: ['x'], courseId: COURSES[0].id, ancestors: [ATTACKER()] })).error.code, 'ext.confirm_in_window');
  assert.equal((await frameCommand(t, { cmd: 'detect', ancestors: [ATTACKER()] })).error.code, 'ext.confirm_in_window');
  assert.deepEqual(await frameCommand(t, { cmd: 'route', ancestors: [ATTACKER()] }), { ok: true, route: 'window', site: ATTACKER() });
  assert.deepEqual(await frameCommand(t, { cmd: 'route', ancestors: ['null'] }), { ok: true, route: 'window', site: '' });
  const conn = await frameCommand(t, { cmd: 'connection', ancestors: [ATTACKER()] });
  assert.equal(conn.connection.status, 'connected');
  assert.equal(conn.connection.courses, undefined, 'no course list for a frame that only shows row states');

  // After 600 ms the window sends, for that frame.
  await win.waitForTimeout(400);
  await win.click('#primary');
  await win.waitForSelector('body[data-view="queued"]', { timeout: 10000 });
  const [enq] = await enqueues();
  assert.equal(enq.sessionId, PANOPTO.deliveryId);
  assert.deepEqual(enq.tracks.map((x) => x.role), ['podcast']);
  assert.ok(enq.referer.startsWith(`http://${PANOPTO.host}:`), 'the embedded page (same origin as the MP4), never the embedding site');
  assert.deepEqual(enq.cookies, []);

  // Trusting the site: the check box, stored as the origin only.
  await win.check('#trust');
  for (let i = 0; i < 50 && Object.keys(await trusted()).length === 0; i++) await win.waitForTimeout(100);
  assert.deepEqual(Object.keys(await trusted()), [ATTACKER()]);
  await win.close();
  for (let i = 0; i < 50 && (await ticket()); i++) await page.waitForTimeout(100);
  assert.equal(await ticket(), null, 'the ticket goes with its window');

  // Trusted now, but the iframe is transparent: the card opens in the page,
  // IntersectionObserver v2 says it is not visible, and the click goes to the window.
  await btn.click();
  const card = frame.locator('lectureflow-ui[data-kind="card"]');
  await card.locator('.lf-card[data-view="ready"]').waitFor({ timeout: 15000 });
  const win2 = await windowFrom(() => clickArmed(card.locator('#primary')));
  await card.waitFor({ state: 'detached', timeout: 5000 });
  await win2.waitForSelector('#trust-line', { timeout: 10000 });
  assert.equal(await win2.isChecked('#trust'), true, 'already trusted');
  assert.equal((await enqueues()).length, 1, 'nothing more was sent from the page');
  await win2.close();
  server.panopto.downloadUrl = null;
  await page.close();
});

test('a trusted site, visible: the card confirms in the page, but only 600 ms after its button is ready', async () => {
  server.panopto.downloadUrl = '/Panopto/Podcast/Download/whole.mp4';
  await setTrusted([CANVAS()]);
  await H.fakeLectureFlow(worker, { nextTaskId: 420 });
  const page = await open(`${CANVAS()}/canvas/embed.html`);
  const frame = page.frameLocator('iframe[name="tool_content"]');
  const btn = frame.locator('h1#title lectureflow-ui[data-kind="player"] .ip-btn');
  await btn.waitFor({ timeout: 10000 });
  await btn.click();
  const card = frame.locator('lectureflow-ui[data-kind="card"]');
  await card.locator('#primary:not([disabled])').waitFor({ timeout: 15000 });
  await card.locator('#primary').evaluate((b) => b.click());
  await page.waitForTimeout(300);
  assert.deepEqual(await enqueues(), [], 'too early: dropped');
  await page.waitForTimeout(400);
  await card.locator('#primary').click();
  await card.locator('.lf-card[data-view="queued"]').waitFor({ timeout: 10000 });
  assert.equal((await enqueues()).length, 1);
  assert.equal(context.pages().filter(isConfirm).length, 0, 'no window');
  server.panopto.downloadUrl = null;
  await page.close();
});

test('a small embedded player (no room for the card): the window, without the click-jacking note on a trusted site, with it on another', async () => {
  server.panopto.downloadUrl = '/Panopto/Podcast/Download/whole.mp4';
  await setTrusted([CANVAS()]);
  await H.fakeLectureFlow(worker, { nextTaskId: 430 });
  // trusted Canvas, a 380 × 214 player: the window, plain
  const page = await open(`${CANVAS()}/canvas/embed.html?small=1`);
  const frame = page.frameLocator('iframe[name="tool_content"]');
  const btn = frame.locator('h1#title lectureflow-ui[data-kind="player"] .ip-btn');
  await btn.waitFor({ timeout: 10000 });
  const win = await windowFrom(() => btn.click());
  await win.waitForSelector('body[data-view="ready"]', { timeout: 15000 });
  assert.equal(await frame.locator('lectureflow-ui[data-kind="card"]').count(), 0, 'no card cut off in the player');
  assert.equal(await win.getAttribute('body', 'data-notice'), 'plain');
  assert.equal(await win.isVisible('#embed-why'), false, 'no click-jacking note on a trusted site');
  assert.equal(await win.isVisible('#trust-line'), false, 'nothing to trust: it is trusted');
  assert.equal((await win.textContent('#embed-from')).trim(), `录播页面：${PANOPTO.host}:${port()}`);
  assert.match(await win.textContent('#lecture'), /CSC1003\/L01\/01/);
  assert.equal((await ticket()).site, CANVAS());
  // the window's own rule holds: a click the moment the button is ready is dropped
  await win.waitForSelector('#primary:not([disabled])', { timeout: 15000 });
  await win.evaluate(() => document.getElementById('primary').click());
  await win.waitForTimeout(300);
  assert.deepEqual(await enqueues(), [], 'the early click was dropped');
  await win.waitForTimeout(400);
  await win.click('#primary');
  await win.waitForSelector('body[data-view="queued"]', { timeout: 10000 });
  const [enq] = await enqueues();
  assert.equal(enq.sessionId, PANOPTO.deliveryId);
  assert.deepEqual(enq.tracks.map((x) => x.role), ['podcast']);
  assert.ok(enq.referer.startsWith(`http://${PANOPTO.host}:`), 'the embedded page, never the embedding site');
  assert.deepEqual(Object.keys(await trusted()), [CANVAS()], 'the list is unchanged');
  await H.shoot(win, 'confirm-window-small');
  await win.close();
  await page.close();

  // the same small player on a site that is not trusted: the note and the offer, as ever
  const page2 = await open(`${ATTACKER()}/canvas/embed.html?small=1`);
  const frame2 = page2.frameLocator('iframe[name="tool_content"]');
  const btn2 = frame2.locator('h1#title lectureflow-ui[data-kind="player"] .ip-btn');
  await btn2.waitFor({ timeout: 10000 });
  const win2 = await windowFrom(() => btn2.click());
  await win2.waitForSelector('body[data-view="ready"]', { timeout: 15000 });
  assert.equal(await win2.getAttribute('body', 'data-notice'), 'warn');
  const why2 = await win2.textContent('#embed-why');
  assert.ok(why2.includes(`嵌在 ${ATTACKER()} 的网页里`), why2);
  assert.equal(await win2.isVisible('#trust-line'), true);
  assert.equal(await win2.isChecked('#trust'), false);
  await win2.close();
  await page2.close();
  server.panopto.downloadUrl = null;
});

test('bulk from a page the extension cannot see: the window lists the rows, the frame reads sessionInfo from its own origin, and checks its rows again when the window closes', async () => {
  await setTrusted([]);
  await H.fakeLectureFlow(worker, {
    checkMap: { [S(2)]: { state: 'imported', taskId: 2, lectureId: '01JE2ELECTURE0000000000002' }, [S(3)]: { state: 'downloading', taskId: 33 } },
    titles: { [COURSES[0].id]: [LIST[3].title.replace(/\//g, ' ')] },
    nextTaskId: 440,
  });
  await worker.evaluate((id) => chrome.storage.local.set({ lastCourseId: id }), COURSES[0].id);
  const page = await open(`${ATTACKER()}/canvas/list.html`);
  const frame = page.frameLocator('iframe[name="tool_content"]');
  const hdr = frame.locator('lectureflow-ui[data-kind="header"] .ip-btn');
  await hdr.waitFor({ timeout: 10000 });
  const before = listInfoRequests().length;
  const win = await windowFrom(() => hdr.click());
  await win.waitForSelector('.lf-card[data-phase="idle"]', { timeout: 15000 });
  assert.equal(await frame.locator('lectureflow-ui[data-kind="card"]').count(), 0);
  assert.equal((await win.textContent('#bulk-summary')).trim(), '共 6 节：2 节已导入（按会话与标题去重），1 节正在刮削，3 节将发送');
  assert.deepEqual(await win.locator('#bulk-rows .bulk-row').evaluateAll((xs) => xs.map((x) => x.dataset.sessionId)), LIST.map((s) => s.id));
  assert.equal(listInfoRequests().length, before, 'nothing requested before the click');
  await H.shoot(win, 'confirm-window-bulk');
  await clickArmed(win.locator('#primary'));
  await clickArmed(win.locator('#override-confirm'));
  await win.waitForSelector('.lf-card[data-phase="done"]', { timeout: 15000 });
  const infos = listInfoRequests().slice(before);
  assert.deepEqual(infos.map((r) => r.deliveryId), [S(1), S(5), S(6)], 'only the sessions sent, after the click');
  assert.ok(infos.every((r) => r.host === PANOPTO.host), 'from the list frame’s own origin');
  assert.deepEqual((await enqueues()).map((m) => [m.sessionId, m.overrideDisabled]), [[S(1), false], [S(6), true]]);
  // the window may only act on its own rows
  const t = await ticket();
  assert.equal(t.flow, 'bulk');
  const checksBefore = (await received()).filter((m) => m.type === 'check').length;
  const closed = win.waitForEvent('close');
  await win.click('#bulk-close');
  await closed;
  for (let i = 0; i < 50 && (await received()).filter((m) => m.type === 'check').length === checksBefore; i++) await page.waitForTimeout(100);
  const again = (await received()).filter((m) => m.type === 'check').slice(checksBefore);
  assert.ok(again.some((m) => m.sessionIds.includes(S(1))), 'the frame checked its rows again');
  await page.close();
});

test('the settings list the trusted embedding sites; removing one is immediate', async () => {
  await setTrusted([CANVAS(), ATTACKER()]);
  const opts = await context.newPage();
  await opts.goto(`chrome-extension://${extensionId}/options/options.html`);
  await opts.waitForSelector(`#trusted .row[data-trusted="${ATTACKER()}"]`);
  assert.equal(await opts.locator('#trusted .row').count(), 2);
  assert.equal(await opts.isVisible('#trusted-empty'), false);
  assert.match(await opts.textContent('#trusted-section'), /信任的嵌入网站/);
  assert.match(await opts.textContent(`#trusted .row[data-trusted="${CANVAS()}"]`), /\d{4}-\d{2}-\d{2} 起信任/);
  await H.shoot(opts, 'options-trusted', opts.locator('#trusted-section'));
  await opts.click(`[data-untrust="${ATTACKER()}"]`);
  await opts.waitForSelector(`#trusted .row[data-trusted="${ATTACKER()}"]`, { state: 'detached' });
  assert.deepEqual(Object.keys(await trusted()), [CANVAS()]);
  await opts.click(`[data-untrust="${CANVAS()}"]`);
  await opts.waitForSelector('#trusted-empty', { state: 'visible' });
  assert.deepEqual(await trusted(), {});
  await opts.close();
});

test('nothing reached any host but the local test server', () => {
  const hosts = new Set(server.requests.map((r) => r.host));
  for (const h of hosts) assert.ok(['127.0.0.1', 'canvas.lf-e2e.test', 'attacker.lf-e2e-x.test', PANOPTO.host].includes(h), h);
});
