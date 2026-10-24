// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// The UI language choice (SPEC-v2 P14.7): which language to start with, the
// list offered on the settings page and in the navigation, and switching;
// and the display of Chinese lecture content (P14.6, zh_display.js), saved
// with it.
//
// The choice is stored on the server (kv ui.locale and ui.zh_display, GET/PUT
// /api/settings/ui) so every browser follows it; "" means not chosen yet,
// and then the language is picked from navigator.languages (i18n.js
// detectLocale) and the Chinese display follows the language. Switching
// takes effect at once (setLocale dispatches "localechange", zh_display.js
// "zhdisplaychange"; app.js re-renders) and is saved afterwards; when the
// save fails the previous choice comes back and a toast says it was not
// saved.

import { api, DEV } from './api.js';
import { LOCALES, PSEUDO, getLocale, setLocale, detectLocale, t } from './i18n.js';
import { toast } from './dom.js';
import { ZH_MODES, prepareZh, syncZh, announceZh, zhSetting } from './zh_display.js';

let saved = null; // last UISettings from the server
let saving = 0; // PUTs in flight: an echo of an older value must not switch back
let choice = 0; // choices made in this page; a server answer older than the last one is ignored

/** The languages offered: P14's six, plus en-XA in development mode (P14.9). */
export function localeChoices() {
  return DEV ? [...LOCALES, { code: PSEUDO, name: PSEUDO }] : LOCALES;
}

/** A language's name in that language itself (P14.7). */
export function localeName(code) {
  const l = localeChoices().find((x) => x.code === code);
  return l ? l.name : code;
}

/** The lang attribute for text written in that language (fonts, P14.5). */
export function langAttr(code) {
  return code === PSEUDO ? 'en' : code;
}

function usable(code) {
  return localeChoices().some((l) => l.code === code);
}

// Follows the server's values (another page changed them). The Chinese
// display is loaded first, so the page re-renders once.
async function apply(ui) {
  if (!ui) return;
  saved = ui;
  if (saving) return;
  const zhd = ZH_MODES.includes(ui.zh_display) ? ui.zh_display : '';
  const switchTo = usable(ui.locale) && ui.locale !== getLocale() ? ui.locale : null;
  await prepareZh({ setting: zhd, locale: switchTo || getLocale() });
  if (saving) return; // the user chose something meanwhile
  if (switchTo) {
    syncZh({ setting: zhd }); // app.js re-renders for the language switch
    await setLocale(switchTo);
  } else if (syncZh({ setting: zhd })) {
    announceZh();
  }
}

/**
 * The language to start with: the saved choice, else the browser's
 * languages. Waits for the server at most timeoutMs (a slow answer is still
 * applied when it comes, unless the user chose something meanwhile). The
 * Chinese display setting read with it is taken too (savedZhDisplay).
 */
export async function initialLocale(timeoutMs = 2500) {
  const req = api.uiSettings({ quiet: true }).catch(() => null);
  let timer = null;
  const timeout = new Promise((resolve) => {
    timer = setTimeout(() => resolve(undefined), timeoutMs);
  });
  const ui = await Promise.race([req, timeout]);
  clearTimeout(timer);
  if (ui === undefined) {
    const at = choice;
    req.then((late) => {
      if (late && choice === at) apply(late);
    });
    return detectLocale();
  }
  if (ui) saved = ui;
  return ui && usable(ui.locale) ? ui.locale : detectLocale();
}

/** ui.zh_display as read at start ("" when unknown or not chosen). */
export function savedZhDisplay() {
  return saved && ZH_MODES.includes(saved.zh_display) ? saved.zh_display : '';
}

/**
 * Re-reads the saved choice (SSE changed settings: another page changed
 * it). An answer is dropped when this page made a choice after the request
 * was sent or is still saving one: it would switch back to the old value.
 */
export async function refreshLocale() {
  if (saving) return;
  const at = choice;
  try {
    const ui = await api.uiSettings({ quiet: true });
    if (choice === at && !saving) await apply(ui);
  } catch {
    // keep the current language
  }
}

// The server's current values to send along with a change (the PUT writes
// both): the last ones read, or read now.
async function current() {
  if (!saved) saved = await api.uiSettings({ quiet: true }).catch(() => null);
  return { locale: (saved && saved.locale) || '', zh_display: (saved && saved.zh_display) || '' };
}

/** The user picked a language: switch now, then save it (P14.7). */
export async function chooseLocale(code) {
  if (!usable(code) || code === getLocale()) return;
  const id = ++choice;
  const prev = getLocale();
  saving++;
  try {
    await prepareZh({ locale: code }); // a Traditional display that follows the language
    await setLocale(code);
    try {
      const cur = await current();
      saved = await api.saveUISettings({ locale: code, zh_display: cur.zh_display }, { quiet: true });
    } catch (e) {
      if (id !== choice) return; // a newer choice decides
      await prepareZh({ locale: prev });
      await setLocale(prev);
      toast(t('i18n.language.save_failed', { error: e.message || t('common.unknown_error') }), 'error');
    }
  } finally {
    saving--;
  }
}

/**
 * The user picked how Chinese lecture content is shown (P14.6): ""
 * (follow the interface language), "simplified", "tw" or "hk". Switches
 * now (after loading the converter), then saves; returns false when the
 * converter could not be loaded (Simplified stays) or the save failed.
 */
export async function chooseZhDisplay(value) {
  if (value !== '' && !ZH_MODES.includes(value)) return false;
  const id = ++choice;
  const prev = zhSetting();
  saving++;
  try {
    const ok = await prepareZh({ setting: value });
    if (syncZh({ setting: value })) announceZh();
    try {
      const cur = await current();
      saved = await api.saveUISettings({ locale: cur.locale, zh_display: value }, { quiet: true });
    } catch (e) {
      if (id === choice) {
        if (syncZh({ setting: prev })) announceZh();
        toast(t('settings.display.zh_save_failed', { error: e.message || t('common.unknown_error') }), 'error');
      }
      return false;
    }
    return ok;
  } finally {
    saving--;
  }
}

// Whether a language has its own catalog file yet (P14.2: one that has none
// is shown through its fallback chain). HEAD requests, remembered.
const catalogKnown = new Map();

/** Resolves to true when web/i18n/{code}.json exists. */
export function hasCatalog(code) {
  if (code === 'zh-CN' || code === 'en' || code === PSEUDO) return Promise.resolve(true);
  if (!catalogKnown.has(code)) {
    catalogKnown.set(
      code,
      fetch(`i18n/${code}.json`, { method: 'HEAD', cache: 'no-cache' }).then(
        (r) => r.ok,
        () => true, // unknown: say nothing
      ),
    );
  }
  return catalogKnown.get(code);
}

/** The language shown where `code` has no text: the next one of its chain with a catalog. */
export async function fallbackName(code) {
  if (code === 'yue-Hant-HK' && (await hasCatalog('zh-TW'))) return localeName('zh-TW');
  return localeName('en');
}
