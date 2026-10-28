// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Sending a lecture to LectureFlow (SPEC-ext 用户流程 3–4, Native Messaging
// 协议): which tracks are ticked by default, the selection rules (one
// enqueue takes at most one audio track and two other tracks — 双机位,
// ext-brief §3),
// which URLs' cookies are read when the user clicks, and the enqueue and
// refresh requests. Pure: the background reads the cookies and talks to the
// native host (src/background/actions.js).

import { urlKey, parseHttpUrl, requestableOrigin, potentiallyTrustworthy } from './url.js';
import { isBlockedUrl, blockedCategory } from './blocklist.js';

export const GENERIC = 'generic';

// Task states (internal/bridge). A session that is "active" cannot be sent
// again; "imported" means the lecture exists; failed and cancelled tasks may
// be sent again (the queue reuses the row).
export const ACTIVE_STATES = Object.freeze(['queued', 'downloading', 'merging', 'importing']);

/** The tracks of a detection as listed: named ones in rule order, then the rest. */
export function listedTracks(result) {
  if (!result) return [];
  if (result.named) return [...result.named.tracks, ...result.named.others];
  return result.tracks || [];
}

/** audio, or video (unknown counts with video: LectureFlow takes one audio and up to two other tracks). */
export function groupOf(track) {
  return track.kind === 'audio' ? 'audio' : 'video';
}

/** Whether a track can be sent at all: not DRM-protected, not from a do-not-handle host. */
export function sendable(track) {
  return !track.protected && !(track.url && isBlockedUrl(track.url));
}

/**
 * The three choices the cards offer instead of ticking tracks (user
 * decision, NOTES "用户决定（ext 文案）" 3):
 *   main  主要音视频: the profile's full track (picture and sound in one,
 *         e.g. Panopto's MP4) when there is one; else the main audio plus
 *         the first picture.
 *   dual  双机位: the main audio plus two pictures; only with two or more.
 *   audio 仅音频: the main audio.
 * presets (profile.presets) names the roles of the full and the main audio
 * tracks; without them the first audio track is the main audio. Pictures
 * are the video tracks in list order. Returns track ids, or null when the
 * choice is not possible here.
 */
export const PRESETS = Object.freeze(['main', 'dual', 'audio']);

export function presetSelection(tracks, preset, presets = null) {
  const ok = tracks.filter(sendable);
  const byRoles = (roles) => {
    for (const r of roles || []) {
      const t = ok.find((x) => x.role === r);
      if (t) return t;
    }
    return null;
  };
  const full = presets ? byRoles(presets.full) : null;
  const audio = (presets ? byRoles(presets.audio) : null) || ok.find((t) => t.kind === 'audio') || null;
  const pictures = ok.filter((t) => t.kind === 'video' && t !== full);
  const ids = (list) => list.filter(Boolean).map((t) => t.id);
  if (preset === 'main') {
    if (full) return [full.id];
    const got = ids([audio, pictures[0]]);
    if (got.length) return got;
    return ok[0] ? [ok[0].id] : null;
  }
  if (preset === 'dual') return pictures.length >= 2 ? ids([audio, pictures[0], pictures[1]]) : null;
  if (preset === 'audio') return audio ? [audio.id] : null;
  return null;
}

/**
 * The ticks a card starts with: "主要音视频", or none when the platform says
 * the recording may be restricted (SPEC-ext 默认不勾选; the user picks, and
 * is reminded once before sending).
 */
export function defaultSelection(tracks, { downloadPolicy = 'unknown', presets = null } = {}) {
  if (downloadPolicy === 'disabled') return [];
  return presetSelection(tracks, 'main', presets) || [];
}

/**
 * Ticks or unticks a track. Ticking a track unticks the other track of its
 * group (video is single choice, so is audio). Protected tracks never tick.
 */
export function toggleSelection(tracks, selected, id, on) {
  const t = tracks.find((x) => x.id === id);
  if (!t) return [...selected];
  if (!on) return selected.filter((x) => x !== id);
  if (!sendable(t)) return [...selected];
  const g = groupOf(t);
  const rest = selected.filter((x) => {
    const o = tracks.find((y) => y.id === x);
    return o && x !== id && groupOf(o) !== g;
  });
  return [...rest, id];
}

/**
 * '' when the selection can be sent, else why not. One audio track; one
 * picture, or two with maxVideo 2 (双机位).
 */
export function selectionError(tracks, ids, { maxVideo = 1 } = {}) {
  if (!Array.isArray(ids) || ids.length === 0) return 'empty';
  const picked = [];
  for (const id of ids) {
    const t = tracks.find((x) => x.id === id);
    if (!t) return 'unknown_track';
    if (!sendable(t)) return 'protected';
    if (picked.includes(t)) continue;
    picked.push(t);
  }
  if (picked.filter((t) => groupOf(t) === 'audio').length > 1) return 'too_many_audio';
  if (picked.filter((t) => groupOf(t) === 'video').length > maxVideo) return 'too_many_video';
  return '';
}

/**
 * The URLs whose cookies are read when the user sends (SPEC-ext: 只读轨道所在
 * 域名的 Cookie). request.cookieUrls of the profile: ["track"] (also the
 * default, and for generic detection) reads the cookies the browser would
 * send to each selected track URL; [] reads none.
 */
export function cookieTargets(request, tracks) {
  const kinds = request && Array.isArray(request.cookieUrls) ? request.cookieUrls : ['track'];
  if (!kinds.includes('track')) return [];
  const out = [];
  for (const t of tracks) {
    const k = urlKey(t.url);
    if (k && !out.includes(k)) out.push(k);
  }
  return out;
}

/**
 * Site access the popup offers ("在此网站自动探测"; security review L4): the
 * page's own origin, and apart from it the hosts whose cookies a send would
 * read — the sendable tracks' hosts other than the page's, when the profile
 * reads cookies at all (cookieUrls "track", also generic detection) — for
 * the user to tick one by one. HTTPS only, as the manifest offers.
 * Returns { page, cookieHosts }.
 */
export function accessOffer(result) {
  if (!result || result.status !== 'ok') return { page: '', cookieHosts: [] };
  const page = requestableOrigin(result.pageUrl);
  if (!page) return { page: '', cookieHosts: [] };
  const hosts = new Set();
  for (const u of cookieTargets(result.request, listedTracks(result).filter(sendable))) {
    const o = requestableOrigin(u);
    if (o && o !== page) hosts.add(o);
  }
  return { page, cookieHosts: [...hosts].sort() };
}

/** A browser cookie (cookies.getAll) as bridge.Cookie. */
export function toBridgeCookie(c) {
  const exp = typeof c.expirationDate === 'number' && Number.isFinite(c.expirationDate) ? Math.floor(c.expirationDate) : 0;
  return {
    name: String(c.name ?? ''),
    value: String(c.value ?? ''),
    domain: String(c.domain ?? ''),
    path: String(c.path || '/'),
    expires: exp > 0 ? exp : 0,
    secure: !!c.secure,
    httpOnly: !!c.httpOnly,
    hostOnly: !!c.hostOnly,
  };
}

/** Cookies of several URLs, each once (name, domain and path). */
export function mergeCookies(lists) {
  const seen = new Set();
  const out = [];
  for (const list of lists) {
    for (const c of list || []) {
      const b = toBridgeCookie(c);
      const k = `${b.name}\u0000${b.domain}\u0000${b.path}`;
      if (seen.has(k)) continue;
      seen.add(k);
      out.push(b);
    }
  }
  return out;
}

/**
 * Referer, as a browser with its default policy (strict-origin-when-cross-
 * origin) would send it for these tracks (security review L2): the page
 * address without its fragment when every track is on the page's origin;
 * only the page's origin when any track is on another; nothing from an
 * HTTPS page to an HTTP track that is not potentially trustworthy (a
 * loopback host is: browsers send it the origin, and so does LectureFlow,
 * internal/fetch refererFor), nor when the profile says not to send it.
 */
export function refererFor(result, tracks = []) {
  if (result.request && result.request.sendReferer === false) return '';
  const u = parseHttpUrl(result.pageUrl);
  if (!u) return '';
  u.hash = '';
  u.username = '';
  u.password = '';
  let crossOrigin = false;
  for (const t of tracks || []) {
    const v = parseHttpUrl(t.url);
    if (!v) continue;
    if (u.protocol === 'https:' && !potentiallyTrustworthy(v)) return '';
    if (v.origin !== u.origin) crossOrigin = true;
  }
  return crossOrigin ? `${u.origin}/` : u.href;
}

/** A track name for people: the profile's label, else "Video · 720p · en". */
export function trackName(track, msg) {
  if (track.label) return track.label;
  const parts = [msg(`kind_${track.kind}`)];
  if (track.quality) parts.push(track.quality);
  if (track.language) parts.push(track.language);
  return parts.join(' · ');
}

function bridgeTrack(t, msg) {
  const out = {
    url: t.url,
    kind: t.kind,
    format: t.format,
    role: t.role || '',
    label: trackName(t, msg).slice(0, 200),
    sizeBytes: t.sizeBytes > 0 ? t.sizeBytes : -1,
  };
  if (t.quality) out.quality = t.quality;
  if (t.selector) out.selector = t.selector;
  return out;
}

/** The selected tracks, in list order. */
export function pickTracks(result, ids) {
  return listedTracks(result).filter((t) => ids.includes(t.id));
}

/**
 * Why a send or "续传" must not go out at all: the page, the tab's
 * top-level page (topUrl) or a ticked track is on a do-not-handle site
 * (src/lib/blocklist.js) — the category, or ''. Detection already keeps
 * such pages and tracks out; this is the last check before cookies are
 * read.
 */
export function sendBlockedCategory(result, ids, topUrl = '') {
  const c = blockedCategory(result && result.pageUrl) || blockedCategory(topUrl);
  if (c) return c;
  for (const t of pickTracks(result || {}, ids || [])) {
    const k = blockedCategory(t.url);
    if (k) return k;
  }
  return '';
}

/** The platform half of the dedup key: the profile id, or "generic". */
export function platformOf(result) {
  return result.profile ? result.profile.id : GENERIC;
}

/**
 * The enqueue request (bridge.EnqueueRequest without type and id). Generic
 * detection reads no page text, so it sends an empty title and LectureFlow
 * names the lecture (lead ruling 9).
 */
export function buildEnqueue({ result, ids, courseId, cookies, overrideDisabled, msg }) {
  const tracks = pickTracks(result, ids);
  const lecture = result.lecture || { title: '', recordedAt: '', durationSec: 0 };
  return {
    profile: result.profile
      ? { id: result.profile.id, version: result.profile.version, reviewed: !!result.profile.reviewed }
      : { id: GENERIC, version: '', reviewed: false },
    platform: platformOf(result),
    sessionId: result.sessionId,
    lecture: {
      title: result.profile ? String(lecture.title || '') : '',
      recordedAt: String(lecture.recordedAt || ''),
      durationSec: Number.isFinite(lecture.durationSec) && lecture.durationSec > 0 ? Math.round(lecture.durationSec) : 0,
    },
    courseId: String(courseId),
    tracks: tracks.map((t) => bridgeTrack(t, msg)),
    referer: refererFor(result, tracks),
    cookies: cookies || [],
    maxConnections: (result.request && result.request.maxConnectionsPerFile) || 0,
    overrideDisabled: result.downloadPolicy === 'disabled' && !!overrideDisabled,
  };
}

/** The refresh request (bridge.RefreshRequest without type and id). */
export function buildRefresh({ result, ids, cookies, msg }) {
  const tracks = pickTracks(result, ids);
  return {
    platform: platformOf(result),
    sessionId: result.sessionId,
    tracks: tracks.map((t) => bridgeTrack(t, msg)),
    referer: refererFor(result, tracks),
    cookies: cookies || [],
  };
}

/**
 * What a check item means for the popup:
 * new | imported | active | needs_refresh | again (failed or cancelled:
 * may be sent again).
 */
export function checkOutcome(item) {
  if (!item) return 'new';
  if (item.state === 'imported') return 'imported';
  if (item.state === 'needs_refresh') return 'needs_refresh';
  if (ACTIVE_STATES.includes(item.state)) return 'active';
  if (item.state === 'failed' || item.state === 'cancelled') return 'again';
  return 'new';
}
