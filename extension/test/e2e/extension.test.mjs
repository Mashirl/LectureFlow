// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// End to end: Playwright's own Chromium (installed under tools/ms-playwright,
// a throw-away profile, never the user's browser) loads dist/chrome-e2e and
// visits the local test site. Nothing here reaches the internet.
//
//   npm run test:e2e   (builds dist/chrome-e2e first)

import { test, before, after } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { startServer } from './server.mjs';
import { chromeExtensionId } from '../../scripts/lib.mjs';
import { validate } from '../../src/lib/jsonschema.js';
import { replayReport, compareReplay } from '../../src/lib/replay.js';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');
const repo = path.resolve(root, '..');
const extDir = path.join(root, 'dist', 'chrome-e2e');
if (!process.env.PLAYWRIGHT_BROWSERS_PATH) process.env.PLAYWRIGHT_BROWSERS_PATH = path.join(repo, 'tools', 'ms-playwright');
const { chromium } = await import('playwright');

let server;
let context;
let worker;
let extensionId;
let userDataDir;

before(async () => {
  assert.ok(fs.existsSync(path.join(extDir, 'manifest.json')), 'run "node scripts/build.mjs --target chrome --e2e" first');
  server = await startServer();
  userDataDir = fs.mkdtempSync(path.join(os.tmpdir(), 'lf-ext-e2e-'));
  context = await chromium.launchPersistentContext(userDataDir, {
    channel: 'chromium',
    headless: true,
    args: [`--disable-extensions-except=${extDir}`, `--load-extension=${extDir}`, '--lang=zh-CN'],
  });
  worker = context.serviceWorkers()[0] || (await context.waitForEvent('serviceworker'));
  extensionId = new URL(worker.url()).host;
  // The worker exists before its module has run; its listeners (webRequest
  // among them) are in place once the test hook is.
  for (let i = 0; i < 100; i++) {
    if ((await worker.evaluate(() => typeof globalThis.__lectureflowTest)) === 'object') break;
    await new Promise((r) => setTimeout(r, 50));
  }
  // Every chrome.runtime.connectNative call is counted and fails like a
  // missing host: the e2e build must make none (src/background/nm.js), and
  // even if it did, it would not reach a host registered on this computer.
  await worker.evaluate(() => {
    globalThis.__connectNativeCalls = 0;
    chrome.runtime.connectNative = function () {
      globalThis.__connectNativeCalls++;
      throw new Error('Specified native messaging host not found.');
    };
  });
});

after(async () => {
  if (context) await context.close();
  if (server) await server.close();
  if (userDataDir) fs.rmSync(userDataDir, { recursive: true, force: true });
});

async function tabIdOf(page) {
  const url = page.url();
  return worker.evaluate(async (u) => {
    const tabs = await chrome.tabs.query({});
    const t = tabs.find((x) => x.url === u);
    return t ? t.id : null;
  }, url);
}

async function detect(tabId) {
  return worker.evaluate(async (id) => {
    const r = await globalThis.__lectureflowTest.detectTab(id, { force: true });
    const view = globalThis.__lectureflowTest.viewOf(r);
    const badge = await chrome.action.getBadgeText({ tabId: id });
    return { status: r.status, summary: r.summary, tracks: view.tracks, badge, authorized: r.authorized };
  }, tabId);
}

async function openPage(name, readyTitle) {
  const page = await context.newPage();
  await page.goto(`${server.origin}/${name}`);
  if (readyTitle) await page.waitForFunction((t) => document.title.startsWith(t), readyTitle, { timeout: 10000 });
  await page.waitForTimeout(500);
  return page;
}

test('the development key gives the extension id the native host allows', () => {
  const { key } = JSON.parse(fs.readFileSync(path.join(root, 'manifest.chrome.json'), 'utf8'));
  assert.equal(extensionId, chromeExtensionId(key));
});

test('plain files: element (with a same-origin iframe), webRequest and HEAD', async () => {
  const page = await openPage('mp4.html');
  const tabId = await tabIdOf(page);
  assert.ok(tabId, 'tab id');
  const r = await detect(tabId);
  assert.equal(r.status, 'ok');
  assert.equal(r.authorized, true);
  assert.equal(r.summary.total, 3, JSON.stringify(r.tracks));
  const byKind = r.tracks.map((t) => `${t.kind}/${t.format}`).sort();
  assert.deepEqual(byKind, ['audio/file', 'audio/file', 'video/file']);
  const video = r.tracks.find((t) => t.kind === 'video');
  assert.equal(video.sizeBytes, 123456);
  assert.deepEqual(video.sources, ['element', 'performance', 'webrequest'].filter((s) => video.sources.includes(s)));
  assert.ok(video.sources.includes('element'));
  assert.ok(video.sources.includes('webrequest'), 'the media request was seen by webRequest');
  const frameAudio = r.tracks.find((t) => t.sizeBytes === 4321);
  assert.ok(frameAudio && frameAudio.sources.includes('element'), 'the audio element in the iframe');
  assert.ok(r.tracks.every((t) => t.protected === false));
  const sizes = r.tracks.filter((t) => t.kind === 'audio').map((t) => t.sizeBytes).sort((a, b) => a - b);
  assert.deepEqual(sizes, [4321, 54321]);
  assert.equal(r.badge, '3');
  await page.close();
});

test('HLS master with an audio group: one video track, one track per audio rendition', async () => {
  const page = await openPage('hls.html', 'hls-ready');
  const tabId = await tabIdOf(page);
  const r = await detect(tabId);
  assert.equal(r.summary.total, 3, JSON.stringify(r.tracks));
  const video = r.tracks.filter((t) => t.kind === 'video');
  const audio = r.tracks.filter((t) => t.kind === 'audio');
  assert.equal(video.length, 1);
  assert.equal(video[0].format, 'hls');
  assert.equal(video[0].quality, '720p');
  assert.equal(audio.length, 2);
  assert.ok(audio.every((t) => t.format === 'hls'));
  assert.deepEqual(audio.map((t) => t.language).sort(), ['en', 'zh']);
  // the variant playlist and the segment the player loaded are not tracks;
  // the master was seen by both the resource-timing and webRequest routes
  assert.deepEqual(video[0].sources, ['performance', 'webrequest']);
  // a rendition the player never requested is known from the master only
  assert.ok(audio.every((t) => !t.sources.includes('element')));

  // The popup, opened on this tab, with no LectureFlow behind the native
  // port (the e2e build never opens a real one: src/background/nm.js):
  // "not connected", with the count.
  const popup = await context.newPage();
  await popup.goto(`chrome-extension://${extensionId}/popup/popup.html?tabId=${tabId}`);
  await popup.waitForSelector('#count[data-count="3"]', { timeout: 15000 });
  assert.match(await popup.textContent('#count'), /探测到 3 条轨道/);
  await popup.waitForFunction(() => document.getElementById('conn').dataset.nmState !== 'checking', null, { timeout: 15000 });
  const nmState = await popup.getAttribute('#conn', 'data-nm-state');
  assert.equal(nmState, 'unavailable');
  assert.equal(await popup.getAttribute('body', 'data-view'), 'disconnected');
  assert.match(await popup.textContent('#conn-chip'), /未连接/);
  assert.equal(await popup.isVisible('#recheck'), true);
  assert.equal(await worker.evaluate(() => globalThis.__connectNativeCalls), 0, 'the e2e build opened no real native port');
  await popup.close();

  // The report: valid, redacted, and it replays to the same result.
  const rep = await worker.evaluate((id) => globalThis.__lectureflowTest.reportFor(id), tabId);
  assert.equal(rep.ok, true, JSON.stringify(rep.findings));
  const schema = JSON.parse(fs.readFileSync(path.join(root, 'schema', 'report.schema.json'), 'utf8'));
  assert.deepEqual(validate(schema, rep.report), []);
  const replay = await replayReport(rep.report);
  assert.deepEqual(compareReplay(rep.report, replay), []);
  await page.close();
});

test('manifests on an origin without permission: resource timing finds them, the page fetches them', async () => {
  const page = await openPage('cdn.html', 'cdn-');
  assert.equal(await page.title(), 'cdn-ready');
  const tabId = await tabIdOf(page);
  const r = await detect(tabId);
  assert.equal(r.summary.total, 2, JSON.stringify(r.tracks));
  assert.deepEqual(r.tracks.map((t) => `${t.kind}/${t.format}/${t.quality}`), ['video/hls/1080p', 'audio/hls/']);
  // webRequest sees nothing on a host the user has not granted
  assert.ok(r.tracks.every((t) => t.sources.join() === 'performance'));
  assert.ok(r.tracks.every((t) => !t.unreadable));
  await page.close();
});

test('EME and DRM-signalled DASH are marked protected', async () => {
  const page = await openPage('eme.html', 'eme-');
  const title = await page.title();
  assert.equal(title, 'eme-ready', 'Clear Key unavailable in this Chromium');
  const tabId = await tabIdOf(page);
  const r = await detect(tabId);
  assert.equal(r.summary.total, 3, JSON.stringify(r.tracks));
  assert.equal(r.summary.protected, 3);
  // SPEC-ext: the badge counts the tracks that can be sent; none here, so no number.
  assert.equal(r.badge, '');
  const kinds = r.tracks.map((t) => `${t.kind}/${t.format}`).sort();
  assert.deepEqual(kinds, ['audio/dash', 'video/dash', 'video/file']);
  await page.close();
});

test('no request to the test site carried a cookie', () => {
  assert.ok(server.requests.length > 0);
  assert.ok(server.requests.every((r) => r.cookie === ''));
});
