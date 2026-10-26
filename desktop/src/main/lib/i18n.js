// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The shell's own few strings (splash, error page, close dialog, tray).
// desktop/locales/{locale}.json, flat keys, for the six interface languages
// of SPEC-v2 P14. The language follows the app's own choice (kv ui.locale,
// GET /api/settings/ui), which the shell remembers in {userData}/locale.json
// for the next splash; until a choice is made the system locale decides,
// with the same rule as the page (web/i18n.js detectLocale).
'use strict';

const fs = require('node:fs');
const path = require('node:path');

const LOCALES_DIR = path.join(__dirname, '..', '..', '..', 'locales');
const FALLBACK = 'en';
const LOCALE_FILE = 'locale.json';

// Fallback chains of P14.2 (the locale itself first), as in web/i18n.js.
const CHAINS = {
  'zh-CN': ['zh-CN', 'en'],
  en: ['en'],
  'zh-TW': ['zh-TW', 'en'],
  'yue-Hant-HK': ['yue-Hant-HK', 'zh-TW', 'en'],
  ru: ['ru', 'en'],
  hi: ['hi', 'en'],
};
const LOCALES = Object.keys(CHAINS);

/** pickLocale maps a system locale (app.getLocale(), ICU) to a shipped locale. */
function pickLocale(systemLocale) {
  const l = typeof systemLocale === 'string' ? systemLocale.replace(/_/g, '-').toLowerCase() : '';
  if (l.startsWith('yue') || l === 'zh-hk' || l.startsWith('zh-hant-hk') || l === 'zh-mo') return 'yue-Hant-HK';
  if (l.startsWith('zh-hant') || l === 'zh-tw') return 'zh-TW';
  if (/^zh\b/.test(l)) return 'zh-CN';
  if (/^ru\b/.test(l)) return 'ru';
  if (/^hi\b/.test(l)) return 'hi';
  return FALLBACK;
}

/** resolveLocale: the app's choice when it is a shipped locale, else the system's. */
function resolveLocale(uiLocale, systemLocale) {
  return LOCALES.includes(uiLocale) ? uiLocale : pickLocale(systemLocale);
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

/** chainMessages(locale) → the locale's messages over its fallback chain. */
function chainMessages(locale, dir = LOCALES_DIR) {
  const out = {};
  for (const l of [...CHAINS[locale]].reverse()) Object.assign(out, readMessages(l, dir));
  return out;
}

/**
 * createShellTranslator({ systemLocale, uiLocale }) → { locale, t, use }.
 * t stays the same function, so the tray, menus and IPC handlers that hold
 * it follow a switch; use(uiLocale) switches and returns whether the
 * locale changed.
 */
function createShellTranslator({ systemLocale, uiLocale = '', dir = LOCALES_DIR } = {}) {
  const fallback = readMessages(FALLBACK, dir);
  let inner = null;
  const self = {
    locale: null,
    t: (key, params) => inner(key, params),
    use(next) {
      const locale = resolveLocale(next, systemLocale);
      if (locale === self.locale) return false;
      self.locale = locale;
      inner = createTranslator(chainMessages(locale, dir), fallback);
      return true;
    },
  };
  self.use(uiLocale);
  return self;
}

/** loadTranslator(systemLocale) → { locale, t } (no app choice: before "ready"). */
function loadTranslator(systemLocale, dir = LOCALES_DIR) {
  const { locale, t } = createShellTranslator({ systemLocale, dir });
  return { locale, t };
}

/** readLocaleMemo(file) → the remembered app choice, or "". */
function readLocaleMemo(file, readFile = (f) => fs.readFileSync(f, 'utf8')) {
  try {
    const v = JSON.parse(readFile(file));
    return v && LOCALES.includes(v.locale) ? v.locale : '';
  } catch {
    return '';
  }
}

/** writeLocaleMemo(file, locale): "" removes the memo; written to a temporary file, then renamed. */
function writeLocaleMemo(file, locale) {
  try {
    if (!LOCALES.includes(locale)) {
      fs.rmSync(file, { force: true });
      return true;
    }
    fs.mkdirSync(path.dirname(file), { recursive: true });
    const tmp = `${file}.tmp`;
    fs.writeFileSync(tmp, `${JSON.stringify({ locale })}\n`);
    fs.renameSync(tmp, file);
    return true;
  } catch {
    return false;
  }
}

module.exports = {
  LOCALES_DIR, LOCALES, LOCALE_FILE, CHAINS,
  pickLocale, resolveLocale, format, createTranslator, readMessages, chainMessages,
  createShellTranslator, loadTranslator, readLocaleMemo, writeLocaleMemo,
};
