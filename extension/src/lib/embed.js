// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// In-page buttons inside another site's page (security review M4, lead
// ruling 2026-10-27; NOTES "决定（ext-sec：extension-builder）"). The in-page
// content script runs in every frame of a granted platform site, so a page
// on any site could embed the platform's player or list in a transparent
// iframe and trick two clicks out of the user (clickjacking). Therefore:
//
//   - the top frame (frameId 0) is the platform's own page: unchanged;
//   - a sub-frame first needs its top-level site: the tab's address when
//     the browser gives it (host access to that page), else the last entry
//     of location.ancestorOrigins that the content script read (Chrome, and
//     Firefox 153 checked — older Firefox has none: unknown). "null" (a
//     no-referrer embedding) is unknown too;
//   - top site on the user's list of trusted embedding sites (Canvas…): the
//     in-page card as before, but a confirming control only acts once it has
//     been shown for ARM_DELAY_MS, and on Chrome only while
//     IntersectionObserver v2 says the card is really visible (otherwise the
//     confirmation moves to the extension's own window);
//   - anything else (unknown, or not trusted): the confirmation happens in
//     the extension's own small window, which offers to trust that site;
//   - a trusted site whose frame has no room for the card (a small embedded
//     player): the extension's window too, without the note and the offer
//     (cardFits, cardPlacement, windowNotice).
//
// The trust list lives in storage.local under TRUST_KEY as
// { [origin]: ISO time }: origins only ("https://lms.example.edu"), no page,
// course or lecture. "对这个平台不再提醒" never lifts any of this. Pure.

export const TRUST_KEY = 'trustedEmbedders';
export const MAX_TRUSTED = 100;
/** How long a confirming control must have been shown before it acts. */
export const ARM_DELAY_MS = 600;

/** "scheme://host[:port]" of an http(s) origin or URL; '' for anything else ("null" included). */
export function originOf(raw) {
  const s = String(raw ?? '').trim();
  if (!s || s === 'null' || s.length > 2048) return '';
  let u;
  try {
    u = new URL(s);
  } catch {
    return '';
  }
  if (u.protocol !== 'http:' && u.protocol !== 'https:') return '';
  if (!u.hostname) return '';
  return u.origin;
}

/**
 * Where a frame sits: { sub, site, source }. sub: not the top frame. site:
 * the top-level page's origin, '' when unknown. tabUrl: sender.tab.url (the
 * browser gives it only with access to the top page); ancestors: the frame's
 * location.ancestorOrigins as the content script read it (nearest first,
 * the top-level page last).
 */
export function frameSite({ frameId, tabUrl = '', ancestors = null } = {}) {
  const sub = !(frameId === 0);
  const fromTab = originOf(tabUrl);
  if (!sub) return { sub: false, site: fromTab, source: fromTab ? 'tab' : '' };
  if (fromTab) return { sub: true, site: fromTab, source: 'tab' };
  if (Array.isArray(ancestors) && ancestors.length > 0 && ancestors.length <= 64) {
    const top = originOf(ancestors[ancestors.length - 1]);
    if (top) return { sub: true, site: top, source: 'ancestors' };
  }
  return { sub: true, site: '', source: '' };
}

/** The stored list, cleaned: valid origins with a valid time, at most MAX_TRUSTED. */
export function trustedMap(raw) {
  const out = {};
  if (!raw || typeof raw !== 'object' || Array.isArray(raw)) return out;
  for (const [origin, at] of Object.entries(raw)) {
    if (Object.keys(out).length >= MAX_TRUSTED) break;
    if (originOf(origin) !== origin || typeof at !== 'string' || Number.isNaN(Date.parse(at))) continue;
    out[origin] = at;
  }
  return out;
}

export function isTrusted(raw, site) {
  const o = originOf(site);
  return !!o && o === site && Object.prototype.hasOwnProperty.call(trustedMap(raw), o);
}

/** The list with this site trusted from now on (unchanged for a bad origin or a full list). */
export function withTrusted(raw, site, now = new Date()) {
  const map = trustedMap(raw);
  const o = originOf(site);
  if (!o || o !== site) return map;
  if (!(o in map) && Object.keys(map).length >= MAX_TRUSTED) return map;
  map[o] = new Date(now).toISOString();
  return map;
}

export function withoutTrusted(raw, site) {
  const map = trustedMap(raw);
  delete map[site];
  return map;
}

/** For the settings page: [{ origin, at }], newest first. */
export function trustedList(raw) {
  return Object.entries(trustedMap(raw))
    .map(([origin, at]) => ({ origin, at }))
    .sort((a, b) => (a.at < b.at ? 1 : a.at > b.at ? -1 : a.origin.localeCompare(b.origin)));
}

/**
 * How an in-page button of this frame confirms: 'inpage' (the top frame:
 * unchanged), 'guarded' (a sub-frame of a trusted site: in the page, with
 * the delay and the visibility check), 'window' (the extension's window).
 */
export function embedRoute({ sub, site }, trusted) {
  if (!sub) return 'inpage';
  return site && isTrusted(trusted, site) ? 'guarded' : 'window';
}

/**
 * The room an in-page card needs in its frame (measured in Chrome with the
 * zh-CN interface, NOTES "决定（ext embed-small）"): the send card is 360 px
 * wide (narrower, it wraps and grows by a sixth) and its ready view 319 px
 * tall; the bulk card's is 315 px plus 33 px per row, whose list scrolls,
 * so 420 px shows three rows. A card in less room is cut off by its frame
 * (a small embedded player: only the title line showed).
 */
export const CARD_ROOM = Object.freeze({ width: 360, single: 320, bulk: 420 });
// src/content/inpage.js positionCard: 8 px from the frame's edges, 6 px
// from the button; below it, unless that leaves less than 260 px and there
// is more room above.
const CARD_EDGE = 8;
const CARD_GAP = 6;
const CARD_BELOW_MIN = 260;

/**
 * Whether the card fits in a frame's viewport next to its button, on the
 * side positionCard puts it. viewWidth / viewHeight: the frame's
 * innerWidth / innerHeight; openerTop / openerBottom: the button's box in
 * that viewport; flow: 'single' | 'bulk'. Unknown sizes do not fit (the
 * window always works).
 */
export function cardFits({ viewWidth, viewHeight, openerTop, openerBottom, flow = 'single' } = {}) {
  if (![viewWidth, viewHeight, openerTop, openerBottom].every(Number.isFinite)) return false;
  if (viewWidth - 2 * CARD_EDGE < CARD_ROOM.width) return false;
  const need = flow === 'bulk' ? CARD_ROOM.bulk : CARD_ROOM.single;
  const below = viewHeight - openerBottom - CARD_GAP - CARD_EDGE;
  const above = openerTop - CARD_GAP - CARD_EDGE;
  const room = below < CARD_BELOW_MIN && above > below ? above : below;
  return room >= need;
}

/**
 * Where an in-page button's card goes (user request 2026-10-27: a small
 * embedded player opens the window instead of a card cut off by its
 * frame): { where: 'inpage' | 'guarded' | 'window', reason }. sub: the
 * button is in a sub-frame; route: embedRoute's answer; fits: cardFits.
 * The top frame (the platform's own page) keeps its card. reason, for a
 * window: 'untrusted' (embedded in a site not trusted, or unknown) or
 * 'small' (a trusted site, but the card has no room).
 */
export function cardPlacement({ sub, route, fits }) {
  if (!sub || route === 'inpage') return { where: 'inpage', reason: '' };
  if (route !== 'guarded') return { where: 'window', reason: 'untrusted' };
  return fits ? { where: 'guarded', reason: '' } : { where: 'window', reason: 'small' };
}

/** Why a confirmation window opened, as its ticket keeps it: 'small', 'invisible' (IntersectionObserver v2), else 'untrusted'. */
export function windowReason(raw) {
  return raw === 'small' || raw === 'invisible' ? raw : 'untrusted';
}

/**
 * What the confirmation window says about where the button was: warn (the
 * click-jacking note, "这个录播页面嵌在 … 的网页里") and offerTrust (the
 * "以后在 … 直接在页面上确认" check box). Only a trusted site whose card had no
 * room gets neither: nothing is wrong there, the window is just bigger.
 * trusted is the background's own reading of the list, not the frame's.
 */
export function windowNotice({ site = '', trusted = false, reason = 'untrusted' } = {}) {
  if (site && trusted && reason === 'small') return { warn: false, offerTrust: false };
  return { warn: true, offerTrust: !!site };
}

/**
 * Whether a click on a confirming control goes through: 'allow', 'wait'
 * (shown for less than delayMs, or visibility not known yet — the click is
 * dropped) or 'window' (the card is not really visible: confirm in the
 * extension's window instead). shownAt / now in milliseconds;
 * checksVisibility: IntersectionObserver v2 is watching (Chrome);
 * visible: its last answer (null before the first).
 */
export function guardDecision({ shownAt, now, delayMs = ARM_DELAY_MS, checksVisibility = false, visible = null }) {
  if (!Number.isFinite(shownAt) || !Number.isFinite(now) || now - shownAt < delayMs) return 'wait';
  if (!checksVisibility) return 'allow';
  if (visible === true) return 'allow';
  if (visible === false) return 'window';
  return 'wait';
}
