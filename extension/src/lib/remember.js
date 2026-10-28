// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// "对这个平台不再提醒" (user decision 2026-10-27): the platforms for which the
// user turned off the reminder before sending a possibly restricted
// recording. Kept in storage.local under RESTRICTED_KEY as
// { [platform id]: ISO time }: the platform (a profile id, never generic
// detection) and when — no course, lecture or page. Every send to such a
// platform then carries the override without the reminder; the settings
// page lists them and can undo each. DRM-protected tracks are unaffected:
// they are never sent. Pure: the callers read and write the storage.

import { GENERIC } from './send.js';

export const RESTRICTED_KEY = 'restrictedRemembered';

const ID = /^[a-z][a-z0-9-]{1,39}$/; // profile.schema.json "id"

/** Whether an id can be remembered: a profile's id, not generic detection. */
export function rememberable(platform) {
  return typeof platform === 'string' && ID.test(platform) && platform !== GENERIC;
}

/** The stored value, cleaned: only valid ids with a valid time. */
export function rememberedMap(raw) {
  const out = {};
  if (!raw || typeof raw !== 'object' || Array.isArray(raw)) return out;
  for (const [id, at] of Object.entries(raw)) {
    if (!rememberable(id) || typeof at !== 'string' || Number.isNaN(Date.parse(at))) continue;
    out[id] = at;
  }
  return out;
}

export function isRemembered(raw, platform) {
  return rememberable(platform) && Object.prototype.hasOwnProperty.call(rememberedMap(raw), platform);
}

/** The map with this platform remembered now (unchanged for generic or a bad id). */
export function withRemembered(raw, platform, now = new Date()) {
  const map = rememberedMap(raw);
  if (!rememberable(platform)) return map;
  map[platform] = new Date(now).toISOString();
  return map;
}

/** The map without this platform: reminded every time again. */
export function withoutRemembered(raw, platform) {
  const map = rememberedMap(raw);
  delete map[platform];
  return map;
}

/**
 * For the settings page: [{ id, name, at }], newest first. names: id →
 * display name (packaged and imported profiles); the id itself otherwise.
 */
export function rememberedList(raw, names = {}) {
  return Object.entries(rememberedMap(raw))
    .map(([id, at]) => ({ id, name: (names && typeof names[id] === 'string' && names[id]) || id, at }))
    .sort((a, b) => (a.at < b.at ? 1 : a.at > b.at ? -1 : a.id.localeCompare(b.id)));
}
