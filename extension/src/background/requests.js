// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The third detection route: a read-only webRequest listener. The browser
// only reports requests to hosts the user has granted, so this sees
// nothing on other sites. Per tab it keeps the media and manifest
// responses (URL, type, status, size) since the last navigation, in memory
// and in storage.session (memory only, survives a service-worker restart,
// gone when the browser closes).
//
// Navigation: the buffer is cleared on the main-frame request, which the
// browser reports in order with the page's own requests (tabs.onUpdated
// may arrive after them). Where the browser gives document ids (Chrome),
// detection also keeps only the requests of the documents it scanned.
// Requests the extension makes itself (manifest fetches and size probes)
// are not counted as the page's.
//
// Do-not-handle sites (src/lib/blocklist.js): a request the browser ties to
// a blocked document or frame (Firefox: documentUrl, originUrl,
// frameAncestors; Chrome: the initiator's origin, for a host blocked as a
// whole) is not recorded at all, whichever host serves it. The initiator
// is kept with the entry so that detection can also drop the requests of
// a frame it found blocked (src/lib/blockedframes.js removes it again).

import { browser } from './env.js';
import { classifyUrl, C_HLS, C_DASH, C_FILE } from '../lib/classify.js';
import { contentRangeTotal } from '../lib/merge.js';
import { urlKey } from '../lib/url.js';
import { blockedCategory, originCategory } from '../lib/blocklist.js';

const MAX_PER_TAB = 300;
const PERSIST_DELAY = 500;
const buffers = new Map(); // tabId → { entries: [], restored: bool }
const persistTimers = new Map();
const own = new Map(); // tabId → Map(urlKey → expiry): requests the extension makes
const OWN_TTL_MS = 30000;
let onCandidate = () => {};

/** Marks a URL the extension is about to fetch in a tab. */
export function expectOwn(tabId, url) {
  const k = urlKey(url);
  if (!k) return;
  if (!own.has(tabId)) own.set(tabId, new Map());
  own.get(tabId).set(k, Date.now() + OWN_TTL_MS);
}

function isOwn(tabId, k) {
  const m = own.get(tabId);
  if (!m) return false;
  const exp = m.get(k);
  if (exp === undefined) return false;
  if (exp < Date.now()) {
    m.delete(k);
    return false;
  }
  return true;
}

const key = (tabId) => `wr:${tabId}`;

function header(headers, name) {
  const h = (headers || []).find((x) => x.name.toLowerCase() === name);
  return h ? h.value || '' : '';
}

function buffer(tabId) {
  let b = buffers.get(tabId);
  if (!b) {
    b = { entries: [], restored: false };
    buffers.set(tabId, b);
  }
  return b;
}

function schedulePersist(tabId) {
  if (persistTimers.has(tabId)) return;
  persistTimers.set(
    tabId,
    setTimeout(() => {
      persistTimers.delete(tabId);
      const b = buffers.get(tabId);
      if (!b) return;
      browser.storage.session.set({ [key(tabId)]: b.entries }).catch(() => {});
    }, PERSIST_DELAY),
  );
}

/** Whether the browser says a request came from a blocked document or frame. */
export function fromBlockedSource(details) {
  if (blockedCategory(details.url)) return true;
  for (const u of [details.documentUrl, details.originUrl]) if (typeof u === 'string' && blockedCategory(u)) return true;
  if (Array.isArray(details.frameAncestors) && details.frameAncestors.some((a) => a && typeof a.url === 'string' && blockedCategory(a.url))) return true;
  return typeof details.initiator === 'string' && originCategory(details.initiator) !== '';
}

/** The origin the request came from: Chrome's initiator, Firefox's document. */
function initiatorOf(details) {
  if (typeof details.initiator === 'string') return details.initiator;
  try {
    return typeof details.documentUrl === 'string' ? new URL(details.documentUrl).origin : '';
  } catch {
    return '';
  }
}

function onHeaders(details) {
  if (details.tabId < 0 || details.method === 'HEAD') return;
  if (fromBlockedSource(details)) return;
  const contentType = header(details.responseHeaders, 'content-type');
  const status = details.statusCode;
  const { cls } = classifyUrl(details.url, { contentType, resourceType: details.type, status });
  if (cls !== C_HLS && cls !== C_DASH && cls !== C_FILE) return;
  const b = buffer(details.tabId);
  const k = urlKey(details.url);
  const i = b.entries.findIndex((e) => urlKey(e.url) === k);
  if (i < 0 && isOwn(details.tabId, k)) return; // our own fetch, not the player's
  const len = Number(header(details.responseHeaders, 'content-length'));
  const entry = {
    url: details.url,
    frameId: details.frameId,
    resourceType: details.type,
    statusCode: status,
    contentType,
    contentLength: Number.isFinite(len) && len > 0 ? len : null,
    contentRangeTotal: contentRangeTotal(header(details.responseHeaders, 'content-range')),
    documentId: details.documentId || null,
    initiator: initiatorOf(details),
  };
  if (i >= 0) {
    // Keep the most informative response (a 200 with a length, or a 206
    // with a total) for a URL the player requests in ranges.
    const old = b.entries[i];
    if (!(old.contentRangeTotal || old.contentLength) || entry.contentRangeTotal || entry.contentLength) b.entries[i] = entry;
  } else {
    if (b.entries.length >= MAX_PER_TAB) return;
    b.entries.push(entry);
  }
  schedulePersist(details.tabId);
  onCandidate(details.tabId);
}

function onMainFrame(details) {
  if (details.tabId >= 0 && details.type === 'main_frame') reset(details.tabId);
}

/** Forgets a tab's requests (navigation, close). */
export function reset(tabId) {
  buffers.set(tabId, { entries: [], restored: true });
  browser.storage.session.remove(key(tabId)).catch(() => {});
}

export function forget(tabId) {
  buffers.delete(tabId);
  own.delete(tabId);
  browser.storage.session.remove(key(tabId)).catch(() => {});
}

/**
 * The requests seen in a tab since its last navigation; with documentIds,
 * only those of these documents (entries without an id are kept).
 */
export async function requestsFor(tabId, documentIds = []) {
  const b = buffer(tabId);
  if (!b.restored) {
    b.restored = true;
    try {
      const stored = (await browser.storage.session.get(key(tabId)))[key(tabId)];
      if (Array.isArray(stored)) {
        const have = new Set(b.entries.map((e) => urlKey(e.url)));
        for (const e of stored) if (!have.has(urlKey(e.url))) b.entries.push(e);
      }
    } catch {
      // nothing stored
    }
  }
  const docs = new Set(documentIds.filter(Boolean));
  return b.entries
    .filter((e) => docs.size === 0 || !e.documentId || docs.has(e.documentId))
    .map(({ documentId, ...rest }) => rest);
}

/** Registers the listeners; call at the top level of the background. */
export function install(candidateCallback) {
  onCandidate = candidateCallback || (() => {});
  const filter = { urls: ['<all_urls>'], types: ['media', 'xmlhttprequest', 'object', 'other'] };
  browser.webRequest.onHeadersReceived.addListener(onHeaders, filter, ['responseHeaders']);
  browser.webRequest.onBeforeRequest.addListener(onMainFrame, { urls: ['<all_urls>'], types: ['main_frame'] });
}
