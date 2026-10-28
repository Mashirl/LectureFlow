// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// "续传" starts with the choice the lecture was sent with. LectureFlow gives a
// needs_refresh task fresh URLs by matching each stored track (role, then
// kind); a 双机位 task (one audio, two pictures) refreshed with "主要音视频"
// lacks a picture and is refused (request.invalid {field: tracks}). The
// check and status answers do not carry a task's tracks (bridge.CheckItem,
// bridge.Status), so the extension remembers, per lecture, which of the
// three choices it was sent with: storage.local PRESET_KEY as
// { [hash]: "main" | "dual" | "audio" }, the hash being the first 32 hex
// digits of SHA-256 over platform and session id — no title, no address,
// no course. A hand-picked selection ("选择轨道…") is not remembered (the
// card then starts with "主要音视频", as before). The newest MAX_ENTRIES are
// kept. Pure: the background reads and writes the storage.

import { PRESETS, presetSelection } from './send.js';
import { sha256Hex } from './url.js';

export const PRESET_KEY = 'sentPresets';
export const MAX_ENTRIES = 200;

const KEY = /^[0-9a-f]{32}$/;

/** The storage key of one lecture: a hash of platform and session id. */
export async function presetKey(platform, sessionId) {
  if (!platform || !sessionId) return '';
  return (await sha256Hex(`${platform}\n${sessionId}`)).slice(0, 32);
}

/**
 * Which of the three choices these track ids are on this session's tracks,
 * or '' for a hand-picked selection.
 */
export function presetOf(tracks, ids, presets = null) {
  const want = [...new Set((ids || []).map(String))].sort();
  for (const p of PRESETS) {
    const got = presetSelection(tracks || [], p, presets);
    if (got && got.length === want.length && [...got].sort().every((x, i) => x === want[i])) return p;
  }
  return '';
}

/** The stored map, cleaned. */
export function presetMap(raw) {
  const out = {};
  if (!raw || typeof raw !== 'object' || Array.isArray(raw)) return out;
  for (const [k, v] of Object.entries(raw)) if (KEY.test(k) && PRESETS.includes(v)) out[k] = v;
  return out;
}

/** The choice a lecture was last sent with, or ''. */
export function presetFor(raw, key) {
  const map = presetMap(raw);
  return KEY.test(key || '') && Object.prototype.hasOwnProperty.call(map, key) ? map[key] : '';
}

/**
 * The map with this lecture's choice, newest last, at most MAX_ENTRIES; a
 * hand-picked selection ('') drops what an earlier send remembered.
 */
export function withPreset(raw, key, preset) {
  const map = presetMap(raw);
  if (!KEY.test(key || '')) return map;
  delete map[key];
  if (!PRESETS.includes(preset)) return map;
  map[key] = preset;
  const keys = Object.keys(map);
  for (const k of keys.slice(0, Math.max(0, keys.length - MAX_ENTRIES))) delete map[k];
  return map;
}
