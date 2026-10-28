// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The thin browser layer every background module shares: the promise-based
// `browser` namespace (webextension-polyfill on Chrome, native on Firefox)
// and what this build is.

import '../vendor/browser-polyfill.js';
import { BUILD } from '../build-info.js';
import { LANGUAGE_KEY, isLanguage, loadCatalog, formatMessage } from '../lib/i18n.js';

export const browser = globalThis.browser;
export { BUILD };

// The language chosen in the settings (src/lib/i18n.js): { code, lang,
// messages }, or null for the browser's own choice. Read again whenever
// the setting changes; `languageReady` settles once the first read is done.
let chosen = null;
let ready = null;

export function refreshLanguage() {
  ready = (async () => {
    let code = '';
    try {
      code = String((await browser.storage.local.get(LANGUAGE_KEY))[LANGUAGE_KEY] || '');
    } catch {
      code = '';
    }
    const catalog = isLanguage(code)
      ? await loadCatalog(code, async (dir) => {
          const res = await fetch(browser.runtime.getURL(`_locales/${dir}/messages.json`));
          if (!res.ok) throw new Error(String(res.status));
          return res.json();
        })
      : null;
    chosen = catalog ? { code, ...catalog } : null;
  })().catch(() => {
    chosen = null;
  });
  return ready;
}

export function languageReady() {
  return ready || refreshLanguage();
}

/** The chosen language's catalog for an in-page script, or null. */
export function chosenCatalog() {
  return chosen ? { lang: chosen.lang, messages: chosen.messages } : null;
}

/** A message in the interface language. */
export function message(key, subs) {
  if (chosen) {
    const s = formatMessage(chosen.messages[key], subs);
    if (s) return s;
  }
  try {
    return browser.i18n.getMessage(key, subs) || key;
  } catch {
    return key;
  }
}

/** chrome | edge | firefox, for hello. */
export function browserName() {
  if (BUILD.target === 'firefox') return 'firefox';
  const ua = globalThis.navigator?.userAgent || '';
  return /\bEdg\//.test(ua) ? 'edge' : 'chrome';
}

export function extensionVersion() {
  return browser.runtime.getManifest().version;
}

export function uiLocale() {
  if (chosen) return chosen.lang;
  try {
    return browser.i18n.getUILanguage() || 'en';
  } catch {
    return 'en';
  }
}

/** Why a native port closed: Chrome puts it in lastError, Firefox on the port. */
export function portError(port) {
  if (port && port.error && port.error.message) return port.error.message;
  const le = globalThis.chrome?.runtime?.lastError;
  return le && le.message ? le.message : '';
}
