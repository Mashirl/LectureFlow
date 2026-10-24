// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// Small helpers shared by the player modules.

import { t, has } from './i18n.js';

// Text for a reason the back end gives as a code (P14.3: chapters' and the
// stream's error), or the text itself from an older back end.
export function codeText(s) {
  if (!s) return '';
  return has('errors.' + s) ? t('errors.' + s) : s;
}

// Clock text for the player: mm:ss, or h:mm:ss for both values when the
// lecture is an hour or longer (the width of "current / total" stays fixed).
export function fmtPlayerTime(ms, totalMs = 0) {
  const s = Math.max(0, Math.floor((Number(ms) || 0) / 1000));
  const two = (n) => String(n).padStart(2, '0');
  const hours = Math.floor(s / 3600);
  if (hours > 0 || (Number(totalMs) || 0) >= 3600000) return `${hours}:${two(Math.floor(s / 60) % 60)}:${two(s % 60)}`;
  return `${two(Math.floor(s / 60))}:${two(s % 60)}`;
}

// The beginning of a sentence for the timeline preview (P6.1: ~60
// characters), cut at a word boundary.
export function sentenceStart(text, n = 60) {
  const s = String(text || '').trim();
  if (s.length <= n) return s;
  const cut = s.lastIndexOf(' ', n);
  return s.slice(0, cut > n * 0.6 ? cut : n).replace(/[\s,;:]+$/, '') + '…';
}

export function readPref(key, fallback) {
  try {
    const v = localStorage.getItem(key);
    return v == null ? fallback : JSON.parse(v);
  } catch {
    return fallback;
  }
}

export function writePref(key, value) {
  try {
    localStorage.setItem(key, JSON.stringify(value));
  } catch {
    // storage unavailable: this page only
  }
}
