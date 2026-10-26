// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// Subtitles of the player (SPEC-v2 P3): the settings (saved in localStorage),
// the cue lookup, the self-drawn subtitle block (band below the picture, or
// overlaid on it) and the CC settings panel. No <track> / ::cue: cues come
// from GET /cues (P4) and are drawn into the DOM.

import { h, clear, append } from './dom.js';
import { t } from './i18n.js';
import { pIcon } from './player_icons.js';
import { zhLang } from './zh_display.js';
import { fmtPercent } from './util.js';

const KEY = 'lectureflow.player.subs';

export const DISPLAYS = ['both', 'zh', 'en', 'off']; // the order of the C key (P3)
export const PLACEMENTS = ['auto', 'band', 'overlay'];
export const SIZES = 5;

// Font size as a share of the stage height, 5 steps (P3);
// Chinese is a little larger than English by default (P10).
const EN_SCALE = [0.022, 0.026, 0.03, 0.035, 0.041];
const ZH_SCALE = [0.025, 0.03, 0.035, 0.041, 0.048];
const EN_PX = [13, 46];
const ZH_PX = [15, 54];
export const LH_EN = 1.28;
export const LH_ZH = 1.38;

const DEFAULTS = {
  display: 'both',
  order: 'en', // 'en' = English on top, 'zh' = Chinese on top
  sizeEn: 2,
  sizeZh: 2,
  placement: 'auto',
  opacity: 0.6, // overlay background
  overlayY: null, // overlay: distance of the block's bottom from the picture's bottom, as a share of its height
};

export function loadSettings() {
  try {
    const v = JSON.parse(localStorage.getItem(KEY) || 'null');
    if (v && typeof v === 'object') {
      const s = { ...DEFAULTS, ...v };
      if (!DISPLAYS.includes(s.display)) s.display = DEFAULTS.display;
      if (s.order !== 'zh') s.order = 'en';
      if (!PLACEMENTS.includes(s.placement)) s.placement = DEFAULTS.placement;
      for (const k of ['sizeEn', 'sizeZh']) s[k] = Math.max(0, Math.min(SIZES - 1, Math.round(Number(s[k]) || 0)));
      s.opacity = Math.max(0, Math.min(1, Number(s.opacity)));
      if (!Number.isFinite(s.opacity)) s.opacity = DEFAULTS.opacity;
      s.overlayY = Number.isFinite(s.overlayY) ? Math.max(0, Math.min(0.85, s.overlayY)) : null;
      return s;
    }
  } catch {
    // storage unavailable or broken: defaults
  }
  return { ...DEFAULTS };
}

export function saveSettings(s) {
  try {
    localStorage.setItem(KEY, JSON.stringify(s));
  } catch {
    // this page only
  }
}

export function fontSizes(s, stageH) {
  const clamp = (v, [lo, hi]) => Math.round(Math.max(lo, Math.min(hi, v)) * 10) / 10;
  return { en: clamp(stageH * EN_SCALE[s.sizeEn], EN_PX), zh: clamp(stageH * ZH_SCALE[s.sizeZh], ZH_PX) };
}

// Lines reserved for the band (P2.1: English 2 + Chinese 2, whatever shows now).
export function reservedLines(s) {
  return { en: s.display === 'both' || s.display === 'en' ? 2 : 0, zh: s.display === 'both' || s.display === 'zh' ? 2 : 0 };
}

// ---- cue lookup (P3: start_ms ≤ t < end_ms, binary search) ----

// Index of the last item whose start_ms ≤ ms, or -1.
export function lastStartingBefore(list, ms) {
  let lo = 0;
  let hi = list.length - 1;
  let ans = -1;
  while (lo <= hi) {
    const mid = (lo + hi) >> 1;
    if (list[mid].start_ms <= ms) {
      ans = mid;
      lo = mid + 1;
    } else hi = mid - 1;
  }
  return ans;
}

export function cueAt(cues, ms) {
  const i = lastStartingBefore(cues, ms);
  return i >= 0 && ms < cues[i].end_ms ? i : -1;
}

// ---- text: words, numbers and formulas are never broken (P3) ----

// Latin runs inside Chinese text ("IND-CCA", "AES-256", "O(n log n)") and
// parenthesised groups in English are kept on one line.
const ZH_LATIN = /[A-Za-z0-9][ -ɏ]*[A-Za-z0-9)\]}%]|[A-Za-z0-9]/g;
const EN_GROUP = /\S*\([^()]{1,24}\)\S*/g;

function textWithNoBreaks(text, re) {
  const out = [];
  let last = 0;
  for (const m of text.matchAll(re)) {
    if (m[0].length < 2 || m[0].length > 24) continue; // long runs may wrap between words
    if (m.index > last) out.push(text.slice(last, m.index));
    out.push(h('span', { class: 'pl-nobr' }, m[0]));
    last = m.index + m[0].length;
  }
  if (last < text.length) out.push(text.slice(last));
  return out;
}

// ---- the subtitle block ----

const FADE_MS = 80; // P3

export function createSubtitleBlock() {
  const lineEn = h('div', { class: 'pl-line pl-line-en', lang: 'en' });
  const lineZh = h('div', { class: 'pl-line pl-line-zh', lang: zhLang() }); // the cue text is converted by the caller (P14.6)
  const inner = h('div', { class: 'pl-sub-inner' }, lineEn, lineZh);
  const el = h('div', { class: 'pl-sub', 'aria-live': 'off' }, inner);
  const shown = { en: null, zh: null };
  const timers = { en: 0, zh: 0 };
  const reduced = window.matchMedia ? window.matchMedia('(prefers-reduced-motion: reduce)') : { matches: false };
  let fitTimer = 0;

  function setLine(which, line, text) {
    text = text || '';
    if (shown[which] === text) return;
    shown[which] = text;
    clearTimeout(timers[which]);
    if (!text) {
      if (reduced.matches) {
        clear(line);
        line.classList.remove('is-in');
        return;
      }
      line.classList.remove('is-in');
      line.classList.add('is-out');
      timers[which] = setTimeout(() => {
        clear(line);
        line.classList.remove('is-out');
      }, FADE_MS);
      return;
    }
    clear(line);
    line.append(h('span', { class: 'pl-text' }, textWithNoBreaks(text, which === 'zh' ? ZH_LATIN : EN_GROUP)));
    line.classList.remove('is-out', 'is-in');
    if (!reduced.matches) {
      void line.offsetWidth; // restart the fade-in
      line.classList.add('is-in');
    }
  }

  // Shrinks the text a little when a cue does not fit the reserved lines
  // (a Chinese chunk of more than two lines, P4.4); never cuts it.
  function fit() {
    el.style.removeProperty('--fit');
    if (!el.classList.contains('is-band')) return;
    let f = 1;
    while (inner.scrollHeight > el.clientHeight + 1 && f > 0.7) {
      f -= 0.08;
      el.style.setProperty('--fit', String(f));
    }
  }

  return {
    el,
    show(cue) {
      setLine('en', lineEn, cue ? cue.en : '');
      setLine('zh', lineZh, cue ? cue.zh : '');
      cancelAnimationFrame(fitTimer);
      fitTimer = requestAnimationFrame(fit);
    },
    refit: fit,
    reset() {
      shown.en = shown.zh = null;
    },
  };
}

// ---- CC panel (P3 subtitle settings) ----

function segmented(label, options, value, onPick, hint) {
  const name = 'pl-' + Math.random().toString(36).slice(2);
  return h(
    'fieldset',
    { class: 'pl-cc-row' },
    h('legend', { class: 'pl-cc-label' }, label, hint ? h('kbd', { class: 'pl-cc-kbd' }, hint) : null),
    h(
      'div',
      { class: 'pl-seg', role: 'radiogroup' },
      options.map(([v, text, title]) =>
        h(
          'label',
          { class: 'pl-seg-opt' + (v === value ? ' is-on' : ''), title: title || text },
          h('input', { type: 'radio', name, value: String(v), checked: v === value, class: 'visually-hidden', onchange: () => onPick(v) }),
          h('span', null, text),
        ),
      ),
    ),
  );
}

// openSubsPanel(anchor, getSettings, onChange(patch), {onClose, isOverlay}) →
// {el, close, refresh}. The panel sits above the CC button inside the
// player (so it also shows in full screen); Esc or a click outside closes it.
// englishOnly() true ("transcribe only", 3.1.0: the lecture has no Chinese
// and gets none until a model provider is set up): the display choice is
// English or off, and the Chinese-only rows (order, Chinese size) are left out.
export function createSubsPanel({ getSettings, onChange, isOverlay, onClose, englishOnly = () => false }) {
  const body = h('div', { class: 'pl-cc-body' });
  const el = h('div', { class: 'pl-cc', role: 'dialog', 'aria-label': t('player.cc.title'), hidden: true, tabindex: '-1' }, h('div', { class: 'pl-cc-head' }, pIcon('captions', { size: 15 }), h('span', null, t('player.cc.title'))), body);

  function sizeRow(label, key) {
    const s = getSettings();
    return segmented(
      label,
      Array.from({ length: SIZES }, (_, i) => [i, String(i + 1), t('player.cc.size_n', { n: i + 1 })]),
      s[key],
      (v) => onChange({ [key]: v }),
    );
  }

  function render() {
    const s = getSettings();
    const enOnly = englishOnly();
    const displays = enOnly
      ? [
          ['en', t('player.cc.display_en')],
          ['off', t('player.cc.display_off')],
        ]
      : [
          ['both', t('player.cc.display_both')],
          ['zh', t('player.cc.display_zh')],
          ['en', t('player.cc.display_en')],
          ['off', t('player.cc.display_off')],
        ];
    clear(body);
    // dom.append: the rows left out for an English-only lecture are null
    append(body, [
      segmented(t('player.cc.display'), displays, enOnly && s.display !== 'off' ? 'en' : s.display, (v) => onChange({ display: v }), 'C'),
      enOnly
        ? null
        : segmented(
            t('player.cc.order'),
            [
              ['en', t('player.cc.order_en')],
              ['zh', t('player.cc.order_zh')],
            ],
            s.order,
            (v) => onChange({ order: v }),
          ),
      enOnly ? null : sizeRow(t('player.cc.size_zh'), 'sizeZh'),
      sizeRow(t('player.cc.size_en'), 'sizeEn'),
      segmented(
        t('player.cc.placement'),
        [
          ['auto', t('player.cc.placement_auto'), t('player.cc.placement_auto_hint')],
          ['band', t('player.cc.placement_band')],
          ['overlay', t('player.cc.placement_overlay')],
        ],
        s.placement,
        (v) => onChange({ placement: v }),
      ),
      h(
        'div',
        { class: 'pl-cc-row' + (isOverlay() ? '' : ' is-dim') },
        h('label', { class: 'pl-cc-label', for: 'pl-cc-opacity' }, t('player.cc.opacity'), h('span', { class: 'pl-cc-val' }, fmtPercent(s.opacity, 0))),
        h('input', {
          id: 'pl-cc-opacity',
          type: 'range',
          class: 'pl-range',
          min: '0',
          max: '100',
          step: '5',
          value: String(Math.round(s.opacity * 100)),
          oninput: (e) => onChange({ opacity: Number(e.target.value) / 100 }),
        }),
        h('p', { class: 'pl-cc-hint' }, isOverlay() ? t('player.cc.overlay_hint') : t('player.cc.opacity_band_hint')),
        s.overlayY != null && isOverlay() ? h('button', { type: 'button', class: 'btn btn-sm btn-ghost', onclick: () => onChange({ overlayY: null }) }, pIcon('rotate-ccw', { size: 13 }), h('span', { class: 'btn-label' }, t('player.cc.reset_position'))) : null,
      ),
    ]);
  }

  // radio names change on every render: keep focus by position instead
  function renderKeepFocus() {
    const a = document.activeElement;
    const inside = a && el.contains(a);
    const all = inside ? Array.from(el.querySelectorAll('input')) : [];
    const i = all.indexOf(a);
    render();
    if (inside && i >= 0) {
      const again = Array.from(el.querySelectorAll('input'))[i];
      if (again) again.focus({ preventScroll: true });
    }
  }

  function onDocDown(e) {
    if (!el.hidden && !el.contains(e.target) && !e.target.closest('[data-cc-toggle]')) close();
  }
  function onKey(e) {
    if (e.key === 'Escape' && !el.hidden) {
      e.preventDefault();
      e.stopPropagation();
      close(true);
    }
  }

  function open() {
    render();
    el.hidden = false;
    document.addEventListener('pointerdown', onDocDown, true);
    el.addEventListener('keydown', onKey);
    const first = el.querySelector('input:checked') || el.querySelector('input');
    if (first) first.focus({ preventScroll: true });
  }

  function close(refocus) {
    if (el.hidden) return;
    el.hidden = true;
    document.removeEventListener('pointerdown', onDocDown, true);
    el.removeEventListener('keydown', onKey);
    if (onClose) onClose(!!refocus);
  }

  return {
    el,
    open,
    close,
    toggle() {
      if (el.hidden) open();
      else close(true);
    },
    get isOpen() {
      return !el.hidden;
    },
    refresh() {
      if (!el.hidden) renderKeepFocus();
    },
  };
}
