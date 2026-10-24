// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const i18n = require('../../src/main/lib/i18n');

const zh = i18n.readMessages('zh-CN');
const en = i18n.readMessages('en');

test('system locale: zh* → zh-CN, everything else → en', () => {
  for (const l of ['zh-CN', 'zh', 'zh-TW', 'zh-HK', 'zh_CN', 'ZH-cn']) assert.equal(i18n.pickLocale(l), 'zh-CN', l);
  for (const l of ['en-US', 'en-GB', 'ru', 'hi', 'ja', '', undefined, 'zhx']) assert.equal(i18n.pickLocale(l), 'en', String(l));
});

test('both locales have the same keys and placeholders, no empty strings', () => {
  assert.deepEqual(Object.keys(zh).sort(), Object.keys(en).sort());
  const ph = (s) => (s.match(/\{\w+\}/g) || []).sort().join(',');
  for (const k of Object.keys(en)) {
    assert.ok(en[k].trim() && zh[k].trim(), k);
    assert.equal(ph(zh[k]), ph(en[k]), `placeholders of ${k}`);
  }
});

test('tone (SPEC §0.1): calm, no exclamation marks', () => {
  for (const [name, msgs] of [['zh-CN', zh], ['en', en]]) {
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
