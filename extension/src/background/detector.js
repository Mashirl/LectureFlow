// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Runs detection on a tab: scans the frames (element and resource-timing
// routes), adds the webRequest route, lets src/lib/detect.js do the rest
// with page-context fetches, then applies the matching profile. Keeps the
// last result per tab in memory for the popup, the badge and the report.

import { browser, BUILD, extensionVersion, browserName, uiLocale } from './env.js';
import { scanFrame, pageFetch } from '../content/scan.js';
import { requestsFor, expectOwn } from './requests.js';
import { allProfiles } from './profiles.js';
import { isAuthorized } from './sites.js';
import { detectTracks, summarize } from '../lib/detect.js';
import { findProfile, selectorReads, applyProfile, sessionIdFor, lectureFor, downloadPolicyFor, readResults, profilePermissionOrigins, GENERIC } from '../lib/profile.js';
import { combineObserved, observeConfig } from '../lib/observe.js';
import { blockedCategory, isBlockedUrl } from '../lib/blocklist.js';
import { screenScan } from '../lib/blockedframes.js';
import { listedTracks, sendable, accessOffer } from '../lib/send.js';
import { buildReport, reportFileName } from '../lib/report.js';
import { parseHttpUrl, originPattern } from '../lib/url.js';
import { tokens } from '../ui/tokens.js';

const MAX_MANIFEST_BYTES = 2 * 1024 * 1024;
const results = new Map(); // tabId, or "tabId:frameId" for one frame → last detection (memory only)
const running = new Map(); // same keys → Promise

const keyOf = (tabId, frameId = 0) => (frameId ? `${tabId}:${frameId}` : tabId);

/** Forgets a tab's detections (its frames' too). */
export function forgetTab(tabId) {
  for (const k of [...results.keys()]) if (k === tabId || String(k).startsWith(`${tabId}:`)) results.delete(k);
}

/** The last detection of a tab, or of one frame of it (an in-page button in an LMS frame). */
export function lastResult(tabId, frameId = 0) {
  return results.get(keyOf(tabId, frameId)) || null;
}

async function inject(tabId, frameIds, func, args) {
  const target = frameIds === 'all' ? { tabId, allFrames: true } : { tabId, frameIds };
  return browser.scripting.executeScript({ target, func, args });
}

/** Scans every frame we may read (or just frameIds); [] when the page is off limits. */
async function scan(tabId, reads, only = null) {
  let res;
  try {
    res = await inject(tabId, only || 'all', scanFrame, [reads]);
  } catch {
    if (only) return { frames: [], error: 'no access' };
    // Some browsers refuse the whole call when one frame is off limits:
    // the top frame alone still tells most.
    try {
      res = await inject(tabId, [0], scanFrame, [reads]);
    } catch (e) {
      return { frames: [], error: String(e && e.message ? e.message : e) };
    }
  }
  const frames = [];
  for (const r of res || []) {
    if (!r || !r.result) continue;
    frames.push({ frameId: r.frameId ?? 0, documentId: r.documentId || null, ...r.result });
  }
  return { frames, error: '' };
}

function makeIo(tabId, frames) {
  const frameOrigin = new Map(frames.map((f) => [f.frameId, parseHttpUrl(f.url)?.origin || '']));
  const pickFrame = (url, frameIds) => {
    const origin = parseHttpUrl(url)?.origin;
    for (const [id, o] of frameOrigin) if (o === origin) return id;
    const known = (frameIds || []).find((id) => frameOrigin.has(id));
    return known ?? 0;
  };
  const viaPage = async (url, frameIds, req) => {
    expectOwn(tabId, url);
    try {
      const res = await inject(tabId, [pickFrame(url, frameIds)], pageFetch, [[req]]);
      return res && res[0] && res[0].result ? res[0].result[0] : null;
    } catch {
      return null;
    }
  };
  // Fallback from the extension itself, only for hosts the user granted.
  const viaBackground = async (url, mode) => {
    expectOwn(tabId, url);
    const pattern = originPattern(url);
    if (!pattern || !(await browser.permissions.contains({ origins: [pattern] }).catch(() => false))) return null;
    try {
      if (mode === 'probe') {
        const r = await fetch(url, { method: 'HEAD', credentials: 'include' });
        const len = Number(r.headers.get('content-length'));
        return { ok: r.ok, status: r.status, contentType: r.headers.get('content-type') || '', size: Number.isFinite(len) && len > 0 ? len : -1 };
      }
      const r = await fetch(url, { credentials: 'include' });
      const text = r.ok ? await r.text() : '';
      return { ok: r.ok && text.length <= MAX_MANIFEST_BYTES, status: r.status, contentType: r.headers.get('content-type') || '', text: text.length <= MAX_MANIFEST_BYTES ? text : '' };
    } catch (e) {
      return { ok: false, status: 0, contentType: '', text: '', size: -1, error: String((e && e.name) || 'error') };
    }
  };
  return {
    async fetchText(url, ctx) {
      const page = await viaPage(url, ctx?.frameIds, { url, mode: 'text', maxBytes: MAX_MANIFEST_BYTES });
      if (page && page.ok) return page;
      return (await viaBackground(url, 'text')) || page || { ok: false, status: 0, contentType: '', text: '', error: 'unreachable' };
    },
    async probeFile(url, ctx) {
      const page = await viaPage(url, ctx?.frameIds, { url, mode: 'probe' });
      if (page && page.ok && page.size > 0) return page;
      return (await viaBackground(url, 'probe')) || page || { ok: false, status: 0, contentType: '', size: -1, error: 'unreachable' };
    },
  };
}

/**
 * Detects the tracks of a tab. Returns the stored result:
 * { status: 'ok' | 'no_access' | 'not_a_page', tabId, at, pageUrl, authorized,
 *   profile, sessionId, sessionSource, lecture, downloadPolicy, tracks,
 *   named, summary, raw }
 */
export function detectTab(tabId, opts = {}) {
  const key = keyOf(tabId, opts.frameId || 0);
  if (running.has(key) && !opts.force) return running.get(key);
  const p = run(tabId, opts).finally(() => running.delete(key));
  running.set(key, p);
  return p;
}

/**
 * opts.frameId (with opts.frameUrl): detect within that frame only — the
 * in-page button of a player embedded in another site's page (an LMS),
 * where the top page is not ours to read.
 */
async function run(tabId, opts = {}) {
  const frameId = opts.frameId || 0;
  const key = keyOf(tabId, frameId);
  let pageUrl = '';
  if (frameId) pageUrl = opts.frameUrl || '';
  else {
    try {
      pageUrl = (await browser.tabs.get(tabId)).url || '';
    } catch {
      return { status: 'no_access', tabId };
    }
  }
  if (!pageUrl) return store(key, { status: 'no_access', tabId });
  const page = parseHttpUrl(pageUrl);
  if (!page) return store(key, { status: 'not_a_page', tabId });
  // YouTube and course platforms: not handled at all (src/lib/blocklist.js).
  const pageBlocked = blockedCategory(pageUrl);
  if (pageBlocked) return unsupported(key, tabId, frameId, pageBlocked);

  const profiles = await allProfiles();
  const only = frameId ? [frameId] : null;
  // First pass: media, resources and frame addresses.
  const first = await scan(tabId, [], only);
  if (first.frames.length === 0) return store(key, { status: 'no_access', tabId, error: first.error });
  // Frames of blocked sites (an embedded course platform, a page that
  // showed one before) and the requests they made are taken out here
  // (src/lib/blockedframes.js); the rest of the tab goes on without them.
  const screened = screenScan({
    frames: first.frames,
    webRequests: (await requestsFor(tabId, first.frames.map((f) => f.documentId))).filter((w) => !frameId || w.frameId === frameId),
  });
  const selfBlocked = screened.frameCategory(frameId || 0);
  if (selfBlocked) return unsupported(key, tabId, frameId, selfBlocked);
  const frameUrls = screened.frames.filter((f) => f.frameId !== 0).map((f) => f.url);
  // Embedded frames the scan could not read (another origin, not granted):
  // their addresses only, to offer the right site access in the popup.
  const opaque = [...new Set(screened.frames.flatMap((f) => f.opaque || []))].slice(0, 20);
  const match = findProfile(profiles, pageUrl, frameUrls);
  let frames = screened.frames.map(({ opaque: _o, ...f }) => f);
  // Second pass for the profile's selectors, only when it has any.
  const reads = match ? selectorReads(match.profile) : [];
  if (reads.length) {
    const second = await scan(tabId, reads, only);
    const byId = new Map(second.frames.map((f) => [f.frameId, f.reads]));
    frames = frames.map((f) => ({ ...f, reads: byId.get(f.frameId) || [] }));
  }
  const webRequests = screened.webRequests;
  frames = frames.map(({ documentId, ...f }) => f);
  const detected = await detectTracks({ frames, webRequests }, makeIo(tabId, frames));
  // Media served from a do-not-handle host (an embedded YouTube player) is not offered.
  const detection = { ...detected, tracks: detected.tracks.filter((t) => !isBlockedUrl(t.url)) };
  const locale = uiLocale();
  const observed = match ? combineObserved(match.profile, frames) : null;
  const named = match ? applyProfile(match.profile, detection.tracks, locale, observed) : null;
  const sel = readResults(frames);
  const pageDuration = detection.evidence.mediaDurations.length ? Math.max(...detection.evidence.mediaDurations) : 0;
  const session = await sessionIdFor(match, pageUrl, sel);
  const cfg = match ? observeConfig(match.profile) : null;
  const result = {
    status: 'ok',
    tabId,
    at: Date.now(),
    pageUrl,
    authorized: await isAuthorized(pageUrl),
    profile: match
      ? { id: match.profile.id, version: match.profile.version, name: match.profile.name, reviewed: match.reviewed, origins: profilePermissionOrigins(match.profile) }
      : null,
    request: match ? { ...(match.profile.request || {}) } : {},
    presets: match ? match.profile.presets || null : null,
    // Whether the profile reads a page response, and whether it was seen
    // (on an authorized site, from the page's first load after granting).
    observe: { declared: !!cfg, seen: !!(observed && observed.seen.size > 0) },
    sessionId: session.sessionId,
    sessionSource: session.source,
    lecture: lectureFor(match, sel, pageDuration, observed),
    downloadPolicy: downloadPolicyFor(match, sel, observed),
    tracks: detection.tracks,
    named,
    summary: null,
    // For the report only; never leaves the background except redacted.
    raw: { frames, webRequests, detection, match, observed },
    opaque,
    // what was left out because it belongs to a blocked site (the popup says so)
    blockedEmbeds: screened.categories,
  };
  result.summary = summarize(listedTracks(result));
  // The badge counts what can be sent (SPEC-ext 用户流程 1): DRM-protected
  // tracks do not, and a page with protected tracks only shows no number.
  if (!frameId) await setBadge(tabId, listedTracks(result).filter(sendable).length);
  return store(key, result);
}

function store(key, result) {
  results.set(key, result);
  return result;
}

/** A blocked page or frame: nothing detected, no number on the badge. */
async function unsupported(key, tabId, frameId, category) {
  if (!frameId) await setBadge(tabId, 0);
  return store(key, { status: 'unsupported', tabId, category });
}

export async function setBadge(tabId, count) {
  try {
    await browser.action.setBadgeText({ tabId, text: count > 0 ? String(count) : '' });
    await browser.action.setBadgeBackgroundColor({ tabId, color: tokens.light['--accent'] });
    if (browser.action.setBadgeTextColor) await browser.action.setBadgeTextColor({ tabId, color: tokens.light['--text-on-accent'] });
  } catch {
    // the tab is gone
  }
}

/** A popup-safe view of a result: no URLs, no raw inputs, no session id. */
export function viewOf(result) {
  if (!result || result.status !== 'ok') return { status: result ? result.status : 'none', category: (result && result.category) || '' };
  const strip = (t) => ({
    id: t.id,
    kind: t.kind,
    format: t.format,
    quality: t.quality || '',
    sizeBytes: t.sizeBytes,
    sizeEstimated: !!t.sizeEstimated,
    sources: t.sources,
    protected: t.protected,
    name: t.name || '',
    language: t.language || '',
    unreadable: !!t.unreadable,
    role: t.role || '',
    label: t.label || '',
    hint: t.hint || '',
    recommended: !!t.recommended,
  });
  return {
    status: 'ok',
    authorized: result.authorized,
    profile: result.profile,
    host: parseHttpUrl(result.pageUrl)?.hostname || '',
    lecture: result.lecture,
    downloadPolicy: result.downloadPolicy,
    observe: result.observe,
    presets: result.presets || null,
    summary: result.summary,
    tracks: listedTracks(result).map(strip),
    platform: result.profile ? result.profile.id : GENERIC,
    blockedEmbeds: Array.isArray(result.blockedEmbeds) ? [...result.blockedEmbeds] : [],
  };
}

/**
 * The origin "在此网站自动探测" asks for: the page's own, nothing more
 * (security review L4; a plain-HTTP page cannot be authorized).
 */
export function suggestedOrigins(result) {
  const { page } = accessOffer(result);
  return page ? [page] : [];
}

/**
 * Hosts whose cookies a send from this page would read (the tracks' hosts
 * other than the page's; none when the profile reads no cookies): listed
 * apart, for the user to tick one by one.
 */
export function cookieHostOrigins(result) {
  return accessOffer(result).cookieHosts;
}

/** Tabs with a detection the options page can export a report of (no addresses, host only). */
export function reportableTabs() {
  const out = [];
  for (const [tabId, r] of results) {
    if (typeof tabId !== 'number' || !r || r.status !== 'ok' || !r.raw) continue;
    out.push({ tabId, host: parseHttpUrl(r.pageUrl)?.hostname || '', total: r.summary ? r.summary.total : 0, profile: r.profile ? r.profile.name : '', at: r.at });
  }
  return out.sort((a, b) => b.at - a.at);
}

/** Builds the redacted report for a tab, re-detecting if needed. */
export async function reportFor(tabId) {
  let result = results.get(tabId);
  if (!result || result.status !== 'ok' || !result.raw) result = await detectTab(tabId, { force: true });
  if (!result || result.status !== 'ok') return { ok: false, reason: result ? result.status : 'none' };
  const { report, findings } = buildReport({
    extension: { version: extensionVersion(), browser: browserName() },
    pageUrl: result.pageUrl,
    frames: result.raw.frames,
    webRequests: result.raw.webRequests,
    detection: result.raw.detection,
    match: result.raw.match,
    observed: result.raw.observed,
    named: result.named,
    session: { source: result.sessionSource },
    lecture: result.lecture,
    downloadPolicy: result.downloadPolicy,
    now: new Date(),
  });
  if (findings.length) return { ok: false, reason: 'personal_data', findings };
  return { ok: true, report, fileName: reportFileName(report) };
}

export { BUILD };
