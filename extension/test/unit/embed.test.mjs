// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// In-page buttons inside other sites' pages (security review M4): where a
// frame sits, the trusted embedding sites (origins and a time only), how a
// button confirms, the delay and visibility rule; and who may open a port
// and what an in-page port hears (L3).

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { TRUST_KEY, MAX_TRUSTED, ARM_DELAY_MS, CARD_ROOM, originOf, frameSite, trustedMap, isTrusted, withTrusted, withoutTrusted, trustedList, embedRoute, guardDecision, cardFits, cardPlacement, windowReason, windowNotice } from '../../src/lib/embed.js';
import { portKindOf, statusForPage } from '../../src/lib/ports.js';

const NOW = new Date('2026-10-27T05:00:00Z');
const CANVAS = 'https://canvas.example.edu';

test('origins: scheme, host and port only; "null" and anything not http(s) is unknown', () => {
  assert.equal(originOf('https://canvas.example.edu/courses/1?x=y#z'), CANVAS);
  assert.equal(originOf('https://canvas.example.edu'), CANVAS);
  assert.equal(originOf('http://lms.example.edu:8080/x'), 'http://lms.example.edu:8080');
  assert.equal(originOf('HTTPS://Canvas.Example.EDU:443/'), CANVAS, 'normalised by the URL parser');
  for (const bad of ['null', '', null, undefined, 'about:blank', 'chrome-extension://abc/x', 'file:///c:/x', 'not a url', `https://${'a'.repeat(3000)}.edu`]) assert.equal(originOf(bad), '', String(bad).slice(0, 30));
});

test('where a frame sits: the top frame; a sub-frame by the tab’s address, else by ancestorOrigins (the last is the top-level page)', () => {
  assert.deepEqual(frameSite({ frameId: 0, tabUrl: 'https://tenant.cloud.panopto.eu/Panopto/Pages/Viewer.aspx?id=1' }), { sub: false, site: 'https://tenant.cloud.panopto.eu', source: 'tab' });
  assert.equal(frameSite({ frameId: 0 }).sub, false, 'the top frame never needs the window');
  // the browser gave the tab's address (access to the top page): it wins
  assert.deepEqual(frameSite({ frameId: 3, tabUrl: `${CANVAS}/courses/1`, ancestors: ['https://evil.example.org'] }), { sub: true, site: CANVAS, source: 'tab' });
  // no access to the top page: the content script's ancestorOrigins
  assert.deepEqual(frameSite({ frameId: 3, tabUrl: '', ancestors: ['https://lti.example.net', CANVAS] }), { sub: true, site: CANVAS, source: 'ancestors' });
  // unknown: no list (older Firefox), an empty or masked one (no-referrer), garbage
  for (const ancestors of [null, undefined, [], ['null'], ['javascript:alert(1)'], 'https://x.example', new Array(65).fill(CANVAS)]) {
    assert.deepEqual(frameSite({ frameId: 3, ancestors }), { sub: true, site: '', source: '' }, JSON.stringify(ancestors)?.slice(0, 40));
  }
});

test('the trusted embedding sites: origins and a time, nothing else; a damaged value is cleaned', () => {
  assert.equal(TRUST_KEY, 'trustedEmbedders');
  const map = withTrusted(undefined, CANVAS, NOW);
  assert.deepEqual(map, { [CANVAS]: '2026-10-27T05:00:00.000Z' });
  assert.equal(isTrusted(map, CANVAS), true);
  assert.equal(isTrusted(map, `${CANVAS}/`), false, 'an origin, exactly');
  assert.equal(isTrusted(map, 'https://evil.example.org'), false);
  assert.equal(isTrusted(map, 'http://canvas.example.edu'), false, 'another scheme is another site');
  // only proper origins can be added
  for (const bad of [`${CANVAS}/courses`, 'null', '', 'canvas.example.edu', 'file:///x']) assert.deepEqual(withTrusted(map, bad, NOW), map, bad);
  assert.deepEqual(trustedMap({ [CANVAS]: NOW.toISOString(), 'https://x.example/path': NOW.toISOString(), 'https://y.example': 'yesterday', null: NOW.toISOString(), course: { name: 'CSC1003' } }), { [CANVAS]: NOW.toISOString() });
  assert.deepEqual(trustedMap(['https://x.example']), {});
  assert.equal(isTrusted(Object.create({ [CANVAS]: NOW.toISOString() }), CANVAS), false, 'own entries only');
  // removal, the list for the settings page (newest first), the cap
  const two = withTrusted(map, 'https://lms.example.edu', new Date('2026-10-28T00:00:00Z'));
  assert.deepEqual(trustedList(two).map((x) => x.origin), ['https://lms.example.edu', CANVAS]);
  assert.deepEqual(withoutTrusted(two, CANVAS), { 'https://lms.example.edu': '2026-10-28T00:00:00.000Z' });
  let full = {};
  for (let i = 0; i < MAX_TRUSTED + 5; i++) full = withTrusted(full, `https://s${i}.example.edu`, NOW);
  assert.equal(Object.keys(full).length, MAX_TRUSTED);
});

test('how a button confirms: the top frame in the page; a trusted site’s frame in the page, guarded; anything else in the window', () => {
  const trusted = withTrusted({}, CANVAS, NOW);
  assert.equal(embedRoute({ sub: false, site: '' }, {}), 'inpage');
  assert.equal(embedRoute({ sub: true, site: CANVAS }, trusted), 'guarded');
  assert.equal(embedRoute({ sub: true, site: 'https://evil.example.org' }, trusted), 'window');
  assert.equal(embedRoute({ sub: true, site: '' }, trusted), 'window', 'unknown site');
  assert.equal(embedRoute({ sub: true, site: CANVAS }, undefined), 'window', 'nothing trusted');
});

test('the card’s room in its frame (CARD_ROOM, measured): 360 px wide, 320 px (send) or 420 px (bulk) beside its button', () => {
  assert.deepEqual({ ...CARD_ROOM }, { width: 360, single: 320, bulk: 420 });
  // a Canvas page's 640 px LMS frame, the button in the title: room below
  assert.equal(cardFits({ viewWidth: 1280, viewHeight: 640, openerTop: 8, openerBottom: 36 }), true);
  // the small embedded player of the user's screenshot (about 380 × 210): no room
  assert.equal(cardFits({ viewWidth: 380, viewHeight: 210, openerTop: 8, openerBottom: 36 }), false);
  // exactly enough below: 36 + 6 + 320 + 8 = 370; one pixel less is not
  assert.equal(cardFits({ viewWidth: 800, viewHeight: 370, openerTop: 8, openerBottom: 36 }), true);
  assert.equal(cardFits({ viewWidth: 800, viewHeight: 369, openerTop: 8, openerBottom: 36 }), false);
  // or above the button, where positionCard puts it when below is short (< 260 px) and above is more
  assert.equal(cardFits({ viewWidth: 800, viewHeight: 400, openerTop: 334, openerBottom: 362 }), true);
  assert.equal(cardFits({ viewWidth: 800, viewHeight: 400, openerTop: 333, openerBottom: 361 }), false);
  assert.equal(cardFits({ viewWidth: 800, viewHeight: 700, openerTop: 400, openerBottom: 428 }), true, '258 px below: above, 386 px');
  assert.equal(cardFits({ viewWidth: 800, viewHeight: 720, openerTop: 400, openerBottom: 428 }), false, '278 px below: positionCard stays below, too little');
  // the width: 360 + 2 × 8
  assert.equal(cardFits({ viewWidth: 376, viewHeight: 900, openerTop: 8, openerBottom: 36 }), true);
  assert.equal(cardFits({ viewWidth: 375, viewHeight: 900, openerTop: 8, openerBottom: 36 }), false);
  // the bulk card needs more
  assert.equal(cardFits({ viewWidth: 800, viewHeight: 400, openerTop: 8, openerBottom: 36, flow: 'bulk' }), false);
  assert.equal(cardFits({ viewWidth: 800, viewHeight: 470, openerTop: 8, openerBottom: 36, flow: 'bulk' }), true);
  // unknown sizes: the window
  assert.equal(cardFits({}), false);
  assert.equal(cardFits({ viewWidth: NaN, viewHeight: 900, openerTop: 8, openerBottom: 36 }), false);
});

test('where a button’s card goes: fits / does not fit × trusted / not trusted; the platform’s own page keeps its card', () => {
  const sub = true;
  // fits, trusted: in the page, guarded (600 ms, IntersectionObserver v2)
  assert.deepEqual(cardPlacement({ sub, route: 'guarded', fits: true }), { where: 'guarded', reason: '' });
  // fits, not trusted: the window, as before
  assert.deepEqual(cardPlacement({ sub, route: 'window', fits: true }), { where: 'window', reason: 'untrusted' });
  // no room, trusted: the window, because it is small
  assert.deepEqual(cardPlacement({ sub, route: 'guarded', fits: false }), { where: 'window', reason: 'small' });
  // no room, not trusted: the window, for the site
  assert.deepEqual(cardPlacement({ sub, route: 'window', fits: false }), { where: 'window', reason: 'untrusted' });
  // an unknown answer counts as not trusted
  assert.deepEqual(cardPlacement({ sub, route: undefined, fits: true }), { where: 'window', reason: 'untrusted' });
  // the top frame (the platform's own page): its card, whatever the room
  for (const fits of [true, false]) {
    assert.deepEqual(cardPlacement({ sub: false, route: 'inpage', fits }), { where: 'inpage', reason: '' });
    assert.deepEqual(cardPlacement({ sub: false, route: 'window', fits }), { where: 'inpage', reason: '' });
  }
});

test('what the window says: the click-jacking note and the trust offer, except for a trusted site’s player that was too small', () => {
  assert.equal(windowReason('small'), 'small');
  assert.equal(windowReason('invisible'), 'invisible');
  for (const raw of ['untrusted', '', undefined, 'inpage', { x: 1 }]) assert.equal(windowReason(raw), 'untrusted');
  // trusted and small: neither
  assert.deepEqual(windowNotice({ site: CANVAS, trusted: true, reason: 'small' }), { warn: false, offerTrust: false });
  // not trusted (small or not): the note and the offer, as before
  assert.deepEqual(windowNotice({ site: CANVAS, trusted: false, reason: 'small' }), { warn: true, offerTrust: true });
  assert.deepEqual(windowNotice({ site: CANVAS, trusted: false, reason: 'untrusted' }), { warn: true, offerTrust: true });
  // trusted, but the card was not really visible (IntersectionObserver v2): the note and the (ticked) offer
  assert.deepEqual(windowNotice({ site: CANVAS, trusted: true, reason: 'invisible' }), { warn: true, offerTrust: true });
  // the frame cannot make a site trusted by saying "small": trusted is the background's reading
  assert.deepEqual(windowNotice({ site: CANVAS, reason: 'small' }), { warn: true, offerTrust: true });
  // an unknown site: the note, nothing to trust
  assert.deepEqual(windowNotice({ site: '', trusted: true, reason: 'small' }), { warn: true, offerTrust: false });
});

test('a confirming control acts only after ARM_DELAY_MS, and (where the browser can tell) only while really visible', () => {
  assert.equal(ARM_DELAY_MS, 600);
  assert.equal(guardDecision({ shownAt: 1000, now: 1100 }), 'wait');
  assert.equal(guardDecision({ shownAt: 1000, now: 1599 }), 'wait');
  assert.equal(guardDecision({ shownAt: 1000, now: 1600 }), 'allow');
  assert.equal(guardDecision({ shownAt: undefined, now: 5000 }), 'wait', 'not seen yet');
  assert.equal(guardDecision({ shownAt: 1000, now: 1700, checksVisibility: true, visible: true }), 'allow');
  assert.equal(guardDecision({ shownAt: 1000, now: 1700, checksVisibility: true, visible: false }), 'window', 'covered or transparent: the window instead');
  assert.equal(guardDecision({ shownAt: 1000, now: 1700, checksVisibility: true, visible: null }), 'wait', 'no answer yet');
  assert.equal(guardDecision({ shownAt: 1000, now: 1100, checksVisibility: true, visible: false }), 'wait', 'too early first');
});

test('ports (L3): popup, options and confirm only from the extension’s own pages; inpage only from a web page in a tab', () => {
  const ID = 'lbfjnfhpnaafingpjkoefobbjnmheehl';
  const pageUrl = (rel) => `chrome-extension://${ID}/${rel}`;
  const env = (frameUrl = '') => ({ extensionId: ID, pageUrl, frameUrl });
  const own = (rel) => ({ id: ID, url: `${pageUrl(rel)}?tabId=7` });
  assert.equal(portKindOf('popup', own('popup/popup.html'), env()), 'popup');
  assert.equal(portKindOf('options', own('options/options.html'), env()), 'options');
  assert.equal(portKindOf('confirm', own('confirm/confirm.html'), env()), 'confirm');
  // a content script naming itself popup / options / confirm
  const content = { id: ID, url: 'https://tenant.cloud.panopto.eu/Panopto/Pages/Embed.aspx?id=1', tab: { id: 7 }, frameId: 3 };
  for (const name of ['popup', 'options', 'confirm']) assert.equal(portKindOf(name, content, env(content.url)), '', name);
  // the wrong page of the extension
  assert.equal(portKindOf('popup', own('options/options.html'), env()), '');
  assert.equal(portKindOf('confirm', own('popup/popup.html'), env()), '');
  // another extension, no sender
  assert.equal(portKindOf('popup', { ...own('popup/popup.html'), id: 'someoneelse' }, env()), '');
  assert.equal(portKindOf('popup', null, env()), '');
  // inpage: a content script in a tab
  assert.equal(portKindOf('inpage', content, env(content.url)), 'inpage');
  assert.equal(portKindOf('inpage', { ...content, tab: undefined }, env(content.url)), '', 'not in a tab');
  assert.equal(portKindOf('inpage', own('popup/popup.html'), env(pageUrl('popup/popup.html'))), '', 'an extension page is not a web page');
  assert.equal(portKindOf('whatever', content, env(content.url)), '');
});

test('an in-page port hears its own platform’s tasks, for the sessions it asked about, without the course (L3)', () => {
  const interest = new Set(['s1']);
  const st = { type: 'status', taskId: 4, platform: 'panopto', sessionId: 's1', courseId: '01JCOURSE', state: 'downloading', bytesDone: 1, bytesTotal: 2 };
  const seen = statusForPage(st, { platform: 'panopto', interest });
  assert.deepEqual(Object.keys(seen).sort(), ['bytesDone', 'bytesTotal', 'platform', 'sessionId', 'state', 'taskId', 'type']);
  assert.equal(statusForPage({ ...st, platform: 'echo360' }, { platform: 'panopto', interest }), null, 'another platform');
  assert.equal(statusForPage({ ...st, sessionId: 's2' }, { platform: 'panopto', interest }), null, 'a session this page never showed');
  assert.equal(statusForPage(st, { platform: '', interest }), null, 'the frame’s profile not known yet');
  assert.equal(statusForPage(null, { platform: 'panopto', interest }), null);
});
