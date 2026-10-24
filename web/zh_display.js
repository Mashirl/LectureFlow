// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// Traditional-Chinese display of lecture content (SPEC-v2 P14.6).
//
// Lecture content is stored in Simplified Chinese and stays so. The
// setting ui.zh_display ("simplified" | "tw" | "hk"; "" = the default of
// the interface language: zh-TW → tw, yue-Hant-HK → hk, others simplified)
// decides how its Chinese parts are SHOWN: translations (transcript,
// player subtitles and transcript), the Chinese titles and blurbs, chapter
// titles and the glossary's Chinese renderings. Converted with OpenCC
// (web/vendor/opencc-js, MIT + Apache-2.0, see its README.md): "cn → twp"
// (Taiwan, with Taiwanese phrasing) and "cn → hk" (Hong Kong). Cantonese is
// only the interface language: lecture text stays written Chinese, only its
// script and regional wording change (P14.6).
//
// Nothing is loaded in Simplified mode. The 1.1 MB cn2t.js is loaded the
// first time a Traditional mode is wanted; t2cn.js (Traditional →
// Simplified, 109 KB) the first time a Chinese search term is typed in a
// Traditional mode.
//
// Views call zh(text) and zhLang() while rendering. A change of the mode
// re-mounts the current view like a language switch (app.js listens to
// "zhdisplaychange"), so no view handles the loading itself: callers that
// know a change is coming (language.js) await prepareZh() first, and a
// change found later (a converter that was not loaded yet) is announced by
// the event once the converter is ready.

const CN2T = 'vendor/opencc-js/cn2t.js';
const T2CN = 'vendor/opencc-js/t2cn.js';

export const ZH_MODES = ['simplified', 'tw', 'hk'];

// OpenCC presets: display (cn → target) and search (target → cn, by
// character, without the phrase conversions of the display direction).
const TO_TRAD = { tw: 'twp', hk: 'hk' };
const TO_SIMP = { tw: 'tw', hk: 'hk' };
const LANG = { simplified: 'zh-CN', tw: 'zh-TW', hk: 'zh-HK' };
const FILE_TAG = { tw: 'zh-Hant-TW', hk: 'zh-Hant-HK' };

// Han characters (CJK unified ideographs, extension A, compatibility):
// text without any is returned as it is, without asking the converter.
const HAN = /[\u3400-\u4dbf\u4e00-\u9fff\uf900-\ufaff]/;

const CACHE_MAX = 20000;

let setting = ''; // ui.zh_display as saved
let locale = 'zh-CN'; // the interface language (for the default)
let mode = 'simplified'; // the mode in effect: its converter is loaded
const toTrad = {}; // mode → converter (display)
const toSimp = {}; // mode → converter (search)
const cache = new Map(); // text → converted text, for the current mode
let failed = false; // the converter could not be loaded: Simplified is shown

/** The display mode a locale implies when nothing is chosen (P14.6). */
export function defaultZhMode(loc) {
  if (loc === 'zh-TW') return 'tw';
  if (loc === 'yue-Hant-HK') return 'hk';
  return 'simplified';
}

function wanted(s = setting, l = locale) {
  return ZH_MODES.includes(s) ? s : defaultZhMode(l);
}

/** ui.zh_display as saved ("" = follow the interface language). */
export function zhSetting() {
  return setting;
}

/** The mode the setting and the interface language ask for (shown as chosen on the settings page). */
export function zhWanted() {
  return wanted();
}

/** The mode in effect: 'simplified' | 'tw' | 'hk'. */
export function zhMode() {
  return mode;
}

/** Whether lecture content is shown in Traditional characters now. */
export function zhOn() {
  return mode !== 'simplified';
}

/** The wanted Traditional mode could not be loaded (Simplified is shown). */
export function zhFailed() {
  return failed && wanted() !== 'simplified' && mode === 'simplified';
}

/** The lang attribute of converted content (fonts, P14.5): zh-CN, zh-TW or zh-HK. */
export function zhLang() {
  return LANG[mode];
}

// ---- loading the UMD builds ----

// Both UMD builds assign globalThis.OpenCC: they are loaded one at a time
// and each library object is taken as soon as its script has run.
let chain = Promise.resolve();

function loadUMD(src) {
  const p = chain.then(
    () =>
      new Promise((resolve, reject) => {
        const s = document.createElement('script');
        s.src = src;
        s.async = true;
        s.onload = () => {
          const lib = window.OpenCC;
          s.remove();
          if (lib && typeof lib.Converter === 'function') resolve(lib);
          else reject(new Error(`${src}: no OpenCC.Converter`));
        };
        s.onerror = () => {
          s.remove();
          reject(new Error(`${src}: not loaded`));
        };
        document.head.append(s);
      }),
  );
  chain = p.catch(() => {});
  return p;
}

const libs = {}; // src → Promise<OpenCC>

function lib(src) {
  if (!libs[src]) {
    const start = performance.now();
    libs[src] = loadUMD(src).then(
      (l) => {
        // the load time is measured for the screenshots / NOTES (first switch)
        try {
          performance.measure(`lectureflow:opencc:${src}`, { start, end: performance.now() });
        } catch {
          // older browsers: no measure options
        }
        return l;
      },
      (e) => {
        delete libs[src]; // try again next time
        throw e;
      },
    );
  }
  return libs[src];
}

let cn2tLib = null; // the loaded cn2t library (converters for both modes come from it)

// The converter of a Traditional mode, made now if cn2t is already loaded.
function tradNow(m) {
  if (!toTrad[m] && cn2tLib) toTrad[m] = cn2tLib.Converter({ from: 'cn', to: TO_TRAD[m] });
  return toTrad[m] || null;
}

async function readyTrad(m) {
  if (m === 'simplified' || tradNow(m)) return true;
  try {
    cn2tLib = await lib(CN2T);
    tradNow(m);
    failed = false;
    return true;
  } catch (e) {
    failed = true;
    console.warn('zh display:', e);
    return false;
  }
}

async function readySimp(m) {
  if (m === 'simplified' || toSimp[m]) return true;
  try {
    const OpenCC = await lib(T2CN);
    toSimp[m] = OpenCC.Converter({ from: TO_SIMP[m], to: 'cn' });
    return true;
  } catch (e) {
    console.warn('zh search:', e);
    return false;
  }
}

// ---- state ----

function announce() {
  window.dispatchEvent(new CustomEvent('zhdisplaychange', { detail: { mode } }));
}

function use(m) {
  if (m === mode) return false;
  mode = m;
  cache.clear();
  return true;
}

/**
 * Loads what the given state would need (the converter of its mode), so
 * that the following syncZh() switches at once. Values not given keep the
 * current ones. Resolves to false when the converter cannot be loaded.
 */
export function prepareZh({ setting: s = setting, locale: l = locale } = {}) {
  return readyTrad(wanted(s, l));
}

/**
 * Takes a new setting and/or interface language. Returns true when the mode
 * in effect changed now (the caller re-renders). When the wanted mode's
 * converter is not loaded yet it is loaded in the background and
 * "zhdisplaychange" is dispatched once it is in effect.
 */
export function syncZh({ setting: s = setting, locale: l = locale } = {}) {
  setting = ZH_MODES.includes(s) ? s : '';
  locale = l || locale;
  const w = wanted();
  if (w === 'simplified' || tradNow(w)) return use(w);
  readyTrad(w).then((ok) => {
    if (ok && wanted() === w && use(w)) announce();
  });
  // until then: the current mode, unless it is another Traditional one
  return mode !== 'simplified' && mode !== w ? use('simplified') : false;
}

/** Re-renders the page after a change made outside a language switch. */
export function announceZh() {
  announce();
}

// ---- conversion ----

/** Lecture content as shown: the text converted to the mode's script (P14.6). */
export function zh(text) {
  if (mode === 'simplified' || text == null || text === '') return text;
  const s = String(text);
  if (!HAN.test(s)) return s;
  let v = cache.get(s);
  if (v === undefined) {
    v = toTrad[mode](s);
    if (cache.size >= CACHE_MAX) cache.clear();
    cache.set(s, v);
  }
  return v;
}

/**
 * Props for an element holding Chinese lecture content: its lang (zh-CN,
 * or zh-TW / zh-HK once converted, P14.5) merged into `props`.
 */
export function zhProps(props = {}) {
  return { ...props, lang: zhLang() };
}

// ---- search (P14.6: Traditional terms find Simplified content) ----

const pendingSearch = new Set();

/**
 * A search query over Chinese lecture content. In a Traditional mode the
 * query as typed is matched against the text as shown, and its Simplified
 * form (t2cn, loaded on the first Chinese query) against the text as
 * stored; `onReady` (a stable function: re-run the search) is called once
 * the Simplified form becomes available. q is expected lower-cased.
 *
 * Returns {q, test(storedText) → bool, needles}: needles are the strings to
 * mark in the text as shown (lower-cased).
 */
export function zhQuery(q, onReady) {
  q = q || '';
  const forms = [q];
  if (mode !== 'simplified' && q && HAN.test(q)) {
    const conv = toSimp[mode];
    if (conv) {
      const s = conv(q);
      if (s !== q) forms.push(s);
    } else {
      if (onReady) pendingSearch.add(onReady);
      const m = mode;
      readySimp(m).then((ok) => {
        const fns = [...pendingSearch];
        pendingSearch.clear();
        if (ok && mode === m) for (const fn of fns) fn();
      });
    }
  }
  const needles = [...new Set([q, ...forms.map((f) => String(zh(f)).toLowerCase())])].filter(Boolean);
  return {
    q,
    needles,
    test(text) {
      if (!q || text == null || text === '') return false;
      const s = String(text);
      const low = s.toLowerCase();
      if (forms.some((f) => low.includes(f))) return true;
      return mode !== 'simplified' && String(zh(s)).toLowerCase().includes(q);
    },
  };
}

/**
 * The text split at the needles (lower-cased search), the matches wrapped
 * by mark(text) → node: [string | node, …]. The earliest match wins, then
 * the longest.
 */
export function markNeedles(text, needles, mark) {
  if (!text || !needles || !needles.length) return [text || ''];
  const low = text.toLowerCase();
  const out = [];
  let i = 0;
  for (;;) {
    let at = -1;
    let len = 0;
    for (const n of needles) {
      if (!n) continue;
      const j = low.indexOf(n, i);
      if (j >= 0 && (at < 0 || j < at || (j === at && n.length > len))) {
        at = j;
        len = n.length;
      }
    }
    if (at < 0) break;
    if (at > i) out.push(text.slice(i, at));
    out.push(mark(text.slice(at, at + len)));
    i = at + len;
  }
  out.push(text.slice(i));
  return out;
}

// ---- downloads (P14.6: exports converted on request) ----

const TEXT_EXT = /\.(md|markdown|srt|vtt|txt|json|csv|tsv)$/i;

/** Whether an export can be offered converted: a text file that may hold Chinese (not *.en.*). */
export function zhConvertible(name) {
  return TEXT_EXT.test(name || '') && !/\.en\.[^.]+$/i.test(name || '');
}

/**
 * The name of a converted download: the variant before the extension
 * ("x.bilingual.md" → "x.bilingual.zh-Hant-TW.md"); a ".zh" before the
 * extension is replaced ("x.zh.vtt" → "x.zh-Hant-TW.vtt").
 */
export function zhFileName(name, m = mode) {
  const tag = FILE_TAG[m];
  if (!tag) return name;
  const dot = name.lastIndexOf('.');
  const base = dot > 0 ? name.slice(0, dot) : name;
  const ext = dot > 0 ? name.slice(dot) : '';
  return (/\.zh$/i.test(base) ? base.slice(0, -3) : base) + '.' + tag + ext;
}

/** The whole text converted (downloads; not cached). */
export function zhConvertAll(text) {
  return mode === 'simplified' || !text ? text : toTrad[mode](text);
}
