// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Sites the user has authorized (optional host permissions). On those the
// badge counts on its own, webRequest and cookies work, and the observer
// content script records media requests from the first moment. Everywhere
// else the extension acts only when the user clicks its icon (activeTab).
//
// On the authorized sites of a profile that observes a page response
// (profile `observe`, src/lib/observe.js), a second, main-world content
// script wraps the page's fetch and XHR (src/content/observe-main.js); it
// is registered on exactly the granted origins that belong to such a
// profile, nowhere else. The in-page buttons follow the same rule for
// profiles with `inpage`.

import { browser } from './env.js';
import { originPattern, parseHttpUrl, hostMatches } from '../lib/url.js';
import { observerMatches } from '../lib/observe.js';
import { inpageMatches } from '../lib/inpage.js';
import { isBlockedUrl, excludeMatchPatterns } from '../lib/blocklist.js';
import { allProfiles } from './profiles.js';

const OBSERVER_ID = 'lf-observe';
const MAIN_OBSERVER_ID = 'lf-observe-main';
const INPAGE_ID = 'lf-inpage';

/** Granted host patterns ("https://example.edu/*", "*://*.panopto.eu/*"). */
export async function grantedOrigins() {
  const all = await browser.permissions.getAll();
  return (all.origins || []).filter((o) => /^(\*|https?):\/\//.test(o));
}

/**
 * Whether the page's host is one the user granted. Never a page of a
 * do-not-handle site (src/lib/blocklist.js): neither a host blocked as a
 * whole nor a blocked path of a host that may otherwise be granted.
 */
export async function isAuthorized(url) {
  const u = parseHttpUrl(url);
  if (!u || isBlockedUrl(url)) return false;
  const pattern = originPattern(url);
  if (!pattern) return false;
  try {
    return await browser.permissions.contains({ origins: [pattern] });
  } catch {
    return false;
  }
}

/** Whether a granted pattern covers a URL (for listing, not for access). */
export function patternCovers(pattern, url) {
  const u = parseHttpUrl(url);
  const m = /^(\*|https?):\/\/([^/]+)\//.exec(pattern);
  if (!u || !m) return false;
  if (m[1] !== '*' && `${m[1]}:` !== u.protocol) return false;
  return hostMatches(m[2], u.hostname);
}

async function registered(id) {
  try {
    return await browser.scripting.getRegisteredContentScripts({ ids: [id] });
  } catch {
    return [];
  }
}

async function upsert(id, def, matches) {
  const existing = await registered(id);
  if (matches.length === 0) {
    if (existing.length) await browser.scripting.unregisterContentScripts({ ids: [id] }).catch(() => {});
    return;
  }
  // Never on a do-not-handle site, whatever was granted (src/lib/blocklist.js).
  const full = { id, runAt: 'document_start', ...def, matches, excludeMatches: excludeMatchPatterns(), allFrames: true, persistAcrossSessions: true };
  try {
    if (existing.length) await browser.scripting.updateContentScripts([full]);
    else await browser.scripting.registerContentScripts([full]);
  } catch {
    // A pattern the browser refuses: detection still works through the
    // scan; the observers come back with the next permission change.
  }
}

/**
 * Keeps the content scripts registered on exactly the granted sites: the
 * isolated-world observer everywhere granted, the main-world response
 * observer only where a profile observes responses, the in-page buttons
 * (src/content/inpage.js) only where a profile has `inpage` — in every
 * frame, so a list or player embedded in an LMS gets them too.
 */
export async function syncObserver() {
  const origins = await grantedOrigins();
  await upsert(OBSERVER_ID, { js: ['content/observe.js'] }, origins);
  let profiles = [];
  try {
    profiles = (await allProfiles()).map((e) => e.profile);
  } catch {
    profiles = [];
  }
  await upsert(MAIN_OBSERVER_ID, { js: ['content/observe-main.js'], world: 'MAIN' }, observerMatches(origins, profiles));
  await upsert(INPAGE_ID, { js: ['content/inpage.js'], runAt: 'document_idle' }, inpageMatches(origins, profiles));
}
