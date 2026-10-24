// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The shell's own few strings (splash, error page, close dialog, tray).
// desktop/locales/{zh-CN,en}.json, flat keys. Step 6 connects the app's
// language setting; until then the system locale decides: zh* → zh-CN,
// everything else → en.
'use strict';

const fs = require('node:fs');
const path = require('node:path');

const LOCALES_DIR = path.join(__dirname, '..', '..', '..', 'locales');
const FALLBACK = 'en';

/** pickLocale maps app.getLocale() to one of the shipped locales. */
function pickLocale(systemLocale) {
  return typeof systemLocale === 'string' && /^zh\b/i.test(systemLocale.replace('_', '-')) ? 'zh-CN' : 'en';
}

function format(template, params) {
  if (!params) return template;
  return template.replace(/\{(\w+)\}/g, (m, k) => (Object.prototype.hasOwnProperty.call(params, k) ? String(params[k]) : m));
}

/**
 * createTranslator(messages, fallbackMessages) → t(key, params). A key
 * missing in both returns the key itself (visible in tests, never blank).
 */
function createTranslator(messages, fallbackMessages = {}) {
  return function t(key, params) {
    const s = Object.prototype.hasOwnProperty.call(messages, key) ? messages[key]
      : Object.prototype.hasOwnProperty.call(fallbackMessages, key) ? fallbackMessages[key] : key;
    return format(s, params);
  };
}

function readMessages(locale, dir = LOCALES_DIR) {
  return JSON.parse(fs.readFileSync(path.join(dir, `${locale}.json`), 'utf8'));
}

/** loadTranslator(systemLocale) → { locale, t }. */
function loadTranslator(systemLocale, dir = LOCALES_DIR) {
  const locale = pickLocale(systemLocale);
  const fallback = readMessages(FALLBACK, dir);
  const messages = locale === FALLBACK ? fallback : readMessages(locale, dir);
  return { locale, t: createTranslator(messages, fallback) };
}

module.exports = { LOCALES_DIR, pickLocale, format, createTranslator, readMessages, loadTranslator };
