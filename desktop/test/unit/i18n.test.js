// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');

const i18n = require('../../src/main/lib/i18n');

const en = i18n.readMessages('en');
const all = Object.fromEntries(i18n.LOCALES.map((l) => [l, i18n.readMessages(l)]));

test('system locale: the same rule as the page (web/i18n.js detectLocale)', () => {
  const cases = {
    'zh-CN': ['zh-CN', 'zh', 'zh_CN', 'ZH-cn', 'zh-Hans-CN', 'zh-SG'],
    'zh-TW': ['zh-TW', 'zh-Hant', 'zh-Hant-TW', 'zh_TW'],
    'yue-Hant-HK': ['zh-HK', 'zh-MO', 'zh-Hant-HK', 'yue', 'yue-Hant-HK'],
    ru: ['ru', 'ru-RU'],
    hi: ['hi', 'hi-IN'],
    en: ['en-US', 'en-GB', 'ja', 'fr-FR', '', undefined, 'zhx', 'rux'],
  };
  for (const [want, inputs] of Object.entries(cases)) {
    for (const l of inputs) assert.equal(i18n.pickLocale(l), want, String(l));
  }
});

test("the app's choice wins over the system locale; unknown choices are ignored", () => {
  assert.equal(i18n.resolveLocale('ru', 'zh-CN'), 'ru');
  assert.equal(i18n.resolveLocale('', 'zh-CN'), 'zh-CN');
  assert.equal(i18n.resolveLocale('en-XA', 'hi-IN'), 'hi');
  assert.equal(i18n.resolveLocale('fr', 'en-US'), 'en');
});

test('every locale has the keys and placeholders of en, no empty strings', () => {
  const ph = (s) => (s.match(/\{\w+\}/g) || []).sort().join(',');
  for (const [name, msgs] of Object.entries(all)) {
    assert.deepEqual(Object.keys(msgs).sort(), Object.keys(en).sort(), name);
    for (const k of Object.keys(en)) {
      assert.ok(msgs[k].trim(), `${name} ${k}`);
      assert.equal(ph(msgs[k]), ph(en[k]), `placeholders of ${name} ${k}`);
    }
  }
});

test('tone (SPEC §0.1): calm, no exclamation marks', () => {
  for (const [name, msgs] of Object.entries(all)) {
    for (const [k, s] of Object.entries(msgs)) assert.doesNotMatch(s, /[!！]/, `${name} ${k}`);
  }
});

test('every key used in the main process exists', () => {
  const src = path.join(__dirname, '..', '..', 'src', 'main');
  const used = new Set();
  for (const f of fs.readdirSync(src)) {
    if (!f.endsWith('.js')) continue;
    const text = fs.readFileSync(path.join(src, f), 'utf8');
    for (const m of text.matchAll(/\bt\('([a-z0-9_.]+)'/g)) used.add(m[1]);
    for (const m of text.matchAll(/title: '([a-z0-9_.]+)'/g)) used.add(m[1]);
  }
  for (const s of ['starting', 'restarting', 'stopping']) used.add(`splash.${s}`);
  assert.ok(used.size > 20);
  for (const k of used) assert.ok(Object.prototype.hasOwnProperty.call(en, k), `missing key ${k}`);
});

test('translator: parameters, fallback to en, missing key shows the key', () => {
  const t = i18n.createTranslator({ a: '你好 {name}' }, { a: 'Hello {name}', b: 'Only English' });
  assert.equal(t('a', { name: 'LF' }), '你好 LF');
  assert.equal(t('b'), 'Only English');
  assert.equal(t('c'), 'c');
  assert.equal(t('a', {}), '你好 {name}');
  const { locale, t: tzh } = i18n.loadTranslator('zh-CN');
  assert.equal(locale, 'zh-CN');
  assert.equal(tzh('action.retry'), '重试');
  assert.equal(i18n.loadTranslator('fr-FR').t('action.retry'), 'Try again');
});

test('shell translator: one t that follows the choice; fallback chains of P14.2', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'lf-i18n-'));
  const write = (l, m) => fs.writeFileSync(path.join(dir, `${l}.json`), JSON.stringify(m));
  write('en', { a: 'A', b: 'B', c: 'C' });
  write('zh-TW', { a: '甲', b: '乙' });
  write('yue-Hant-HK', { a: '呀' });
  write('ru', { a: 'А' });
  const tr = i18n.createShellTranslator({ systemLocale: 'en-US', dir });
  const { t } = tr;
  assert.equal(tr.locale, 'en');
  assert.equal(tr.use(''), false);
  assert.equal(tr.use('yue-Hant-HK'), true);
  assert.deepEqual([tr.locale, t('a'), t('b'), t('c')], ['yue-Hant-HK', '呀', '乙', 'C']);
  assert.equal(tr.use('ru'), true);
  assert.deepEqual([t('a'), t('b')], ['А', 'B']);
  assert.equal(tr.use(''), true);
  assert.equal(t('a'), 'A');
  fs.rmSync(dir, { recursive: true, force: true });
});

test('locale memo: written atomically, "" or unknown removes it, junk reads as ""', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'lf-memo-'));
  const file = path.join(dir, 'sub', i18n.LOCALE_FILE);
  assert.equal(i18n.readLocaleMemo(file), '');
  assert.ok(i18n.writeLocaleMemo(file, 'hi'));
  assert.equal(i18n.readLocaleMemo(file), 'hi');
  assert.ok(!fs.existsSync(`${file}.tmp`));
  assert.ok(i18n.writeLocaleMemo(file, ''));
  assert.ok(!fs.existsSync(file));
  assert.equal(i18n.readLocaleMemo(file, () => '{"locale":"xx"}'), '');
  assert.equal(i18n.readLocaleMemo(file, () => 'not json'), '');
  fs.rmSync(dir, { recursive: true, force: true });
});
