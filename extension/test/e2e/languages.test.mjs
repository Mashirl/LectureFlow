// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// End to end, the six interface languages (SPEC-ext 界面设计, M4): which
// _locales directory Chromium picks by itself for a Hong Kong or Cantonese
// interface (it has none of its own), and the language chosen in the
// extension's settings, in the popup, the in-page button and the settings
// page, each laid out within the popup's 360 px. One browser at a time.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import * as H from './harness.mjs';
import { PANOPTO } from './server.mjs';
import { UI_LANGUAGES, chainOf, mergeCatalogs, formatMessage } from '../../src/lib/i18n.js';

const catalog = (code) => mergeCatalogs(chainOf(code).map((d) => JSON.parse(fs.readFileSync(path.join(H.root, '_locales', d, 'messages.json'), 'utf8'))));
const msg = (code, key, subs) => formatMessage(catalog(code)[key], subs);
const profile = JSON.parse(fs.readFileSync(path.join(H.root, 'profiles', 'panopto.json'), 'utf8'));
const podcastLabel = (bcp47) => profile.tracks.find((r) => r.role === 'podcast').label[bcp47];

/** What Chromium itself makes of a --lang: its interface language and the directory it reads. */
async function chromiumChoice(lang) {
  const B = await H.startBrowser({ lang });
  try {
    return await B.worker.evaluate(() => ({ ui: chrome.i18n.getUILanguage(), send: chrome.i18n.getMessage('sendButton'), name: chrome.i18n.getMessage('extName') }));
  } finally {
    await B.close();
  }
}

test('Chromium by itself: zh-HK reads _locales/zh_HK (Cantonese) though its own interface is zh-TW; zh-MO, zh-Hant-HK read zh_TW; yue has no interface and falls back', async () => {
  // A Hong Kong interface language: Chromium shows its zh-TW interface but
  // gives extensions the requested locale first, so zh_HK is used.
  const hk = await chromiumChoice('zh-HK');
  assert.equal(hk.ui, 'zh-TW');
  assert.equal(hk.send, msg('zh_HK', 'sendButton'));
  assert.equal(hk.name, msg('zh_HK', 'sendButton'), 'the name is the in-page button’s words');
  // no directory of their own: the interface locale (zh-TW) is next
  assert.equal((await chromiumChoice('zh-MO')).send, msg('zh_TW', 'sendButton'));
  assert.equal((await chromiumChoice('zh-Hant-HK')).send, msg('zh_TW', 'sendButton'));
  // "yue" is no Chromium language at all: the system's English, then default_locale
  const yue = await chromiumChoice('yue');
  assert.equal(yue.send, msg('en', 'sendButton'), `yue → ${yue.ui}`);
  assert.equal((await chromiumChoice('ru-RU')).send, msg('ru', 'sendButton'));
});

test('the language chosen in the settings: popup, in-page button, settings page; nothing wider than the popup', async () => {
  const B = await H.startBrowser({ viewport: { width: 1280, height: 800 } });
  const { server, context, worker, extensionId } = B;
  try {
    server.panopto.downloadUrl = '/Panopto/Podcast/Download/whole.mp4';
    await H.fakeLectureFlow(worker);
    const setLanguage = (code) =>
      worker.evaluate(async (c) => {
        if (c) await chrome.storage.local.set({ uiLanguage: c });
        else await chrome.storage.local.remove('uiLanguage');
        await new Promise((r) => setTimeout(r, 150)); // the background reads it again
      }, code);
    const page = await context.newPage();
    await page.goto(`http://${PANOPTO.host}:${server.port}/Panopto/Pages/Viewer.aspx?id=${PANOPTO.deliveryId}`);
    await page.waitForSelector('body[data-ready="1"]', { timeout: 10000 });
    const tabId = await H.tabIdOf(worker, page);

    for (const { code, bcp47 } of [...UI_LANGUAGES, { code: '', bcp47: 'zh-CN' }]) {
      await setLanguage(code);
      const popup = await context.newPage();
      await popup.setViewportSize({ width: 360, height: 600 });
      await popup.goto(`chrome-extension://${extensionId}/popup/popup.html?tabId=${tabId}`);
      await popup.waitForSelector('body[data-view="ready"]', { timeout: 15000 });
      const dir = code || 'zh_CN';
      assert.equal(await popup.getAttribute('html', 'lang'), bcp47, dir);
      assert.equal((await popup.textContent('#primary')).trim(), msg(dir, 'sendButton'), dir);
      assert.equal((await popup.textContent('#presets [data-preset="main"]')).trim(), msg(dir, 'presetMain'), dir);
      assert.ok((await popup.textContent('#chosen')).startsWith(podcastLabel(bcp47)), dir);
      assert.ok((await popup.textContent('#data-note')).includes(msg(dir, 'dataNote').slice(0, 12)), dir);
      // nothing sticks out of the 360 px popup, no text is clipped
      const over = await popup.evaluate(() => {
        const out = [];
        if (document.documentElement.scrollWidth > 360) out.push(`page ${document.documentElement.scrollWidth}`);
        for (const n of document.querySelectorAll('button, .chip, label, h1, h2, h3, p, span')) {
          if (!n.offsetParent || getComputedStyle(n).display === 'inline') continue;
          if (n.scrollWidth > n.clientWidth + 1) out.push(`${n.tagName}#${n.id}.${n.className} width ${n.scrollWidth}>${n.clientWidth}`);
          // a button of fixed height whose label wrapped (a glyph's overhang of a pixel or two is not)
          if (n.tagName === 'BUTTON' && n.scrollHeight > n.clientHeight + 3) out.push(`${n.tagName}#${n.id}.${n.className} height ${n.scrollHeight}>${n.clientHeight}`);
        }
        return out;
      });
      assert.deepEqual(over, [], dir);
      await H.shoot(popup, `lang-popup-${dir}`);
      await popup.close();
    }

    // the in-page button follows the setting (it reads it when the page loads)
    await setLanguage('zh_HK');
    await page.reload();
    await page.waitForSelector('body[data-ready="1"]', { timeout: 10000 });
    const btn = page.locator('lectureflow-ui[data-kind="player"] .ip-btn');
    await btn.waitFor({ timeout: 10000 });
    assert.equal((await btn.textContent()).trim(), msg('zh_HK', 'sendButton'));
    assert.equal(await page.locator('lectureflow-ui[data-kind="player"] .lf-root').getAttribute('lang'), 'yue-Hant-HK');
    await btn.click();
    const card = page.locator('lectureflow-ui[data-kind="card"]');
    await card.locator('.lf-card[data-view="ready"]').waitFor({ timeout: 15000 });
    assert.equal((await card.locator('#primary').textContent()).trim(), msg('zh_HK', 'confirmSend'));
    assert.ok((await card.locator('#chosen').textContent()).startsWith('完整錄影（MP4）'));
    await H.shoot(page, 'lang-inpage-zh_HK', null, { fullPage: false });
    await card.locator('#ip-close').click();

    // the settings page: the browser's language or one of the six, each in its own name
    const opts = await context.newPage();
    await opts.goto(`chrome-extension://${extensionId}/options/options.html`);
    await opts.waitForSelector('#language option', { state: 'attached' });
    assert.deepEqual(await opts.$$eval('#language option', (xs) => xs.map((x) => x.value)), ['', 'zh_CN', 'en', 'zh_TW', 'zh_HK', 'ru', 'hi']);
    assert.deepEqual(await opts.$$eval('#language option', (xs) => xs.slice(1).map((x) => x.textContent)), UI_LANGUAGES.map((l) => l.name));
    assert.equal(await opts.inputValue('#language'), 'zh_HK');
    assert.equal((await opts.textContent('h1')).trim(), msg('zh_HK', 'optionsTitle'));
    await Promise.all([opts.waitForEvent('load'), opts.selectOption('#language', 'ru')]);
    await opts.waitForSelector('#language option', { state: 'attached' });
    assert.equal((await opts.textContent('#language-section h2')).trim(), msg('ru', 'languageHeading'));
    assert.equal(await worker.evaluate(async () => (await chrome.storage.local.get('uiLanguage')).uiLanguage), 'ru');
    await H.shoot(opts, 'lang-options-ru', null, { fullPage: false });
    await Promise.all([opts.waitForEvent('load'), opts.selectOption('#language', '')]);
    await opts.waitForSelector('#language option', { state: 'attached' });
    assert.equal((await opts.textContent('#language-section h2')).trim(), msg('zh_CN', 'languageHeading'));
    assert.equal(await worker.evaluate(async () => 'uiLanguage' in (await chrome.storage.local.get('uiLanguage'))), false, 'the browser’s again: nothing stored');
    await opts.close();
    await page.close();
  } finally {
    server.panopto.downloadUrl = null;
    await B.close();
  }
});
