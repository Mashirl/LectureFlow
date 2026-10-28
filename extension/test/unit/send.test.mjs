// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Sending (SPEC-ext 用户流程 3–4): the default ticks, at most one audio and
// one video, which URLs' cookies are read, the enqueue and refresh
// requests; and the popup's choice of view (five states plus 续传).

import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  defaultSelection, toggleSelection, selectionError, cookieTargets, toBridgeCookie, mergeCookies, buildEnqueue, buildRefresh,
  checkOutcome, refererFor, trackName, listedTracks, groupOf, presetSelection, accessOffer,
} from '../../src/lib/send.js';
import { popupView, progressOf, isFinal } from '../../src/lib/popupstate.js';
import { isBlockedHost, isBlockedUrl, wildcardTouchesBlocked } from '../../src/lib/blocklist.js';

const msg = (k) => ({ kind_video: 'Video', kind_audio: 'Audio', kind_unknown: 'Unknown' })[k] || k;

const V1 = { id: 'v1', url: 'https://cdn.example.net/v1.mp4', kind: 'video', format: 'file', sizeBytes: 100, protected: false, quality: '720p' };
const V2 = { id: 'v2', url: 'https://cdn.example.net/hls/master.m3u8', kind: 'video', format: 'hls', sizeBytes: -1, protected: false };
const A1 = { id: 'a1', url: 'https://cdn.example.net/a1.m4a', kind: 'audio', format: 'file', sizeBytes: 50, protected: false, language: 'en' };
const A2 = { id: 'a2', url: 'https://cdn.example.net/a2.m4a', kind: 'audio', format: 'file', sizeBytes: 40, protected: false };
const U1 = { id: 'u1', url: 'https://cdn.example.net/u1', kind: 'unknown', format: 'hls', sizeBytes: -1, protected: false, unreadable: true };
const P1 = { id: 'p1', url: 'https://cdn.example.net/drm.mpd#lf=video::0', kind: 'video', format: 'dash', sizeBytes: -1, protected: true, selector: 'video::0' };

test('three choices instead of ticking (user decision): 主要音视频 / 双机位 / 仅音频', () => {
  const presets = { full: ['podcast'], audio: ['primary-audio'] };
  const S1 = { ...V1, id: 's1', role: 'screen' };
  const S2 = { ...V2, id: 's2', role: 'screen' };
  const POD = { ...V1, id: 'pod', role: 'podcast' };
  const AUD = { ...A1, id: 'aud', role: 'primary-audio' };
  // Panopto with the full MP4: main is the MP4 alone
  const withPod = [POD, AUD, S1, S2];
  assert.deepEqual(presetSelection(withPod, 'main', presets), ['pod']);
  assert.deepEqual(presetSelection(withPod, 'dual', presets), ['aud', 's1', 's2'], 'audio and two pictures, the MP4 is not a picture');
  assert.deepEqual(presetSelection(withPod, 'audio', presets), ['aud']);
  // without it: audio + the first picture; one picture → no 双机位
  assert.deepEqual(presetSelection([AUD, S1], 'main', presets), ['aud', 's1']);
  assert.equal(presetSelection([AUD, S1], 'dual', presets), null);
  assert.equal(presetSelection([POD, S1], 'audio', presets), null, 'no separate audio');
  // generic: the first audio track is the audio; a muxed file alone is main
  assert.deepEqual(presetSelection([V1, A1, A2], 'main', null), ['a1', 'v1']);
  assert.deepEqual(presetSelection([V1], 'main', null), ['v1']);
  assert.deepEqual(presetSelection([P1, V1, V2, A1], 'dual', null), ['a1', 'v1', 'v2'], 'protected tracks never count');
  assert.equal(presetSelection([P1], 'main', null), null);
  // the starting ticks: main, or nothing when the platform says restricted
  assert.deepEqual(defaultSelection(withPod, { presets }), ['pod']);
  assert.deepEqual(defaultSelection(withPod, { presets, downloadPolicy: 'disabled' }), []);
  assert.deepEqual(defaultSelection([V1, A1], {}), ['a1', 'v1']);
  // 双机位 sends one audio and two pictures; the advanced list keeps one
  assert.equal(selectionError(withPod, ['aud', 's1', 's2'], { maxVideo: 2 }), '');
  assert.equal(selectionError(withPod, ['aud', 's1', 's2']), 'too_many_video');
});

test('ticking: video and audio are each single choice; protected never ticks', () => {
  const all = [V1, V2, A1, A2, U1, P1];
  let s = ['v1', 'a1'];
  s = toggleSelection(all, s, 'v2', true);
  assert.deepEqual(s.sort(), ['a1', 'v2']);
  s = toggleSelection(all, s, 'u1', true);
  assert.deepEqual(s.sort(), ['a1', 'u1'], 'unknown counts with video');
  s = toggleSelection(all, s, 'a2', true);
  assert.deepEqual(s.sort(), ['a2', 'u1']);
  s = toggleSelection(all, s, 'p1', true);
  assert.deepEqual(s.sort(), ['a2', 'u1']);
  s = toggleSelection(all, s, 'a2', false);
  assert.deepEqual(s, ['u1']);
  assert.equal(groupOf(U1), 'video');
});

test('what can be sent: at least one, known, unprotected, one audio and one video at most', () => {
  const all = [V1, V2, A1, A2, P1];
  assert.equal(selectionError(all, ['v1', 'a1']), '');
  assert.equal(selectionError(all, []), 'empty');
  assert.equal(selectionError(all, ['zz']), 'unknown_track');
  assert.equal(selectionError(all, ['p1']), 'protected');
  assert.equal(selectionError(all, ['v1', 'v2']), 'too_many_video');
  assert.equal(selectionError(all, ['a1', 'a2']), 'too_many_audio');
  assert.equal(selectionError([{ ...V1, url: 'https://rr1.googlevideo.com/x.mp4' }], ['v1']), 'protected', 'a do-not-handle host is never sent');
});

test('cookies: of the ticked tracks’ URLs only (cookieUrls ["track"], the default); none for []', () => {
  assert.deepEqual(cookieTargets({}, [V1, A1]), ['https://cdn.example.net/v1.mp4', 'https://cdn.example.net/a1.m4a']);
  assert.deepEqual(cookieTargets(undefined, [V1, V1]), ['https://cdn.example.net/v1.mp4']);
  assert.deepEqual(cookieTargets({ cookieUrls: ['track'] }, [P1]), ['https://cdn.example.net/drm.mpd'], 'without the fragment');
  assert.deepEqual(cookieTargets({ cookieUrls: [] }, [V1, A1]), []);
});

test('cookies become bridge.Cookie, each once', () => {
  const c = { name: 'sid', value: 'v', domain: '.example.net', path: '/', secure: true, httpOnly: true, hostOnly: false, expirationDate: 1893456000.7, sameSite: 'lax', storeId: '0' };
  assert.deepEqual(toBridgeCookie(c), { name: 'sid', value: 'v', domain: '.example.net', path: '/', expires: 1893456000, secure: true, httpOnly: true, hostOnly: false });
  assert.equal(toBridgeCookie({ name: 's', value: 'x', domain: 'a', session: true }).expires, 0);
  const merged = mergeCookies([[c], [c, { ...c, path: '/x' }]]);
  assert.deepEqual(merged.map((x) => x.path), ['/', '/x']);
});

const result = (over = {}) => ({
  status: 'ok',
  pageUrl: 'https://tenant.cloud.panopto.eu/Panopto/Pages/Viewer.aspx?id=abc#t=10',
  profile: { id: 'panopto', version: '1.1.0', name: 'Panopto', reviewed: true },
  request: { sendReferer: true, cookieUrls: [], maxConnectionsPerFile: 4 },
  sessionId: 'abc',
  lecture: { title: 'CSC1003 L1', recordedAt: '2026-10-21T09:35', durationSec: 3299.6 },
  downloadPolicy: 'enabled',
  tracks: [V1, A1, P1],
  named: { tracks: [{ ...V1, role: 'podcast', label: 'Full recording (MP4)', recommended: true }], others: [A1, P1] },
  ...over,
});

test('the enqueue request', () => {
  const r = result();
  assert.deepEqual(listedTracks(r).map((t) => t.id), ['v1', 'a1', 'p1']);
  const cookies = [{ name: 'a', value: 'b', domain: 'cdn.example.net', path: '/', expires: 0, secure: true, httpOnly: true, hostOnly: true }];
  const e = buildEnqueue({ result: r, ids: ['a1', 'v1'], courseId: '01JCOURSE', cookies, overrideDisabled: true, msg });
  assert.deepEqual(e.profile, { id: 'panopto', version: '1.1.0', reviewed: true });
  assert.equal(e.platform, 'panopto');
  assert.equal(e.sessionId, 'abc');
  assert.deepEqual(e.lecture, { title: 'CSC1003 L1', recordedAt: '2026-10-21T09:35', durationSec: 3300 });
  assert.equal(e.courseId, '01JCOURSE');
  assert.deepEqual(e.tracks, [
    { url: V1.url, kind: 'video', format: 'file', role: 'podcast', label: 'Full recording (MP4)', sizeBytes: 100, quality: '720p' },
    { url: A1.url, kind: 'audio', format: 'file', role: '', label: 'Audio · en', sizeBytes: 50 },
  ]);
  assert.equal(e.referer, 'https://tenant.cloud.panopto.eu/', 'tracks on another origin: the page’s origin only (strict-origin-when-cross-origin)');
  assert.deepEqual(e.cookies, cookies);
  assert.equal(e.maxConnections, 4);
  assert.equal(e.overrideDisabled, false, 'only when downloads are off');
  assert.equal(buildEnqueue({ result: result({ downloadPolicy: 'disabled' }), ids: ['v1'], courseId: 'c', overrideDisabled: true, msg }).overrideDisabled, true);
  // Generic detection: empty title (LectureFlow names it), "generic", no connection cap.
  const g = buildEnqueue({ result: result({ profile: null, request: {}, named: null, lecture: { title: 'ignored', recordedAt: '', durationSec: 0 } }), ids: ['p1', 'v1'].slice(1), courseId: 'c', msg });
  assert.deepEqual(g.profile, { id: 'generic', version: '', reviewed: false });
  assert.equal(g.platform, 'generic');
  assert.equal(g.lecture.title, '');
  assert.equal(g.maxConnections, 0);
  assert.equal(g.tracks[0].label, 'Video · 720p');
  // A DASH track carries its selector.
  assert.equal(buildEnqueue({ result: result(), ids: ['p1'], courseId: 'c', msg }).tracks[0].selector, 'video::0');
  assert.equal(refererFor(result({ request: { sendReferer: false } })), '');
  assert.equal(trackName({ kind: 'unknown' }, msg), 'Unknown');
});

test('the refresh request', () => {
  const r = buildRefresh({ result: result(), ids: ['v1'], cookies: [], msg });
  assert.deepEqual(Object.keys(r).sort(), ['cookies', 'platform', 'referer', 'sessionId', 'tracks']);
  assert.equal(r.tracks[0].role, 'podcast');
  assert.equal(r.referer, 'https://tenant.cloud.panopto.eu/', 'the same rule as the enqueue');
});

test('Referer as a browser’s default policy would send it (security review L2)', () => {
  const page = 'https://user:pw@tenant.cloud.panopto.eu/Panopto/Pages/Viewer.aspx?id=abc&token=SECRET#t=10';
  const same = { url: 'https://tenant.cloud.panopto.eu/Panopto/Podcast/x.mp4?sig=1' };
  const cdn = { url: 'https://d0000.cloudfront.net/sessions/x/master.m3u8' };
  const plainHttp = { url: 'http://media.example.edu/x.mp4' };
  const r = { request: {}, pageUrl: page };
  // every track on the page's origin: the address without fragment or user info
  assert.equal(refererFor(r, [same]), 'https://tenant.cloud.panopto.eu/Panopto/Pages/Viewer.aspx?id=abc&token=SECRET');
  // any track elsewhere: the origin only, no path, no query
  assert.equal(refererFor(r, [cdn]), 'https://tenant.cloud.panopto.eu/');
  assert.equal(refererFor(r, [same, cdn]), 'https://tenant.cloud.panopto.eu/', 'mixed: the stricter one');
  // another port or scheme is another origin
  assert.equal(refererFor(r, [{ url: 'https://tenant.cloud.panopto.eu:8443/x.mp4' }]), 'https://tenant.cloud.panopto.eu/');
  // HTTPS page, HTTP track: nothing (no-referrer-when-downgrade part of the policy)
  assert.equal(refererFor(r, [plainHttp]), '');
  assert.equal(refererFor(r, [same, plainHttp]), '');
  // ... but an HTTP loopback track is potentially trustworthy (Secure
  // Contexts): browsers send it the origin, as LectureFlow does
  // (internal/fetch refererFor; the native end-to-end test's media is there)
  for (const url of ['http://127.0.0.1:8080/x.mp4', 'http://127.9.9.9/x.mp4', 'http://[::1]:9000/x.mp4', 'http://localhost/x.mp4', 'http://media.localhost./x.mp4']) {
    assert.equal(refererFor(r, [{ url }]), 'https://tenant.cloud.panopto.eu/', url);
    assert.equal(refererFor(r, [same, { url }]), 'https://tenant.cloud.panopto.eu/', `mixed: ${url}`);
  }
  for (const url of ['http://10.0.0.1/x.mp4', 'http://128.0.0.1/x.mp4', 'http://localhost.example.edu/x.mp4', 'http://[::2]/x.mp4']) {
    assert.equal(refererFor(r, [{ url }]), '', url);
  }
  assert.equal(refererFor(r, [{ url: 'http://127.0.0.1/x.mp4' }, plainHttp]), '', 'one untrustworthy HTTP track: nothing');
  // an HTTP page to HTTPS media elsewhere: the origin
  assert.equal(refererFor({ request: {}, pageUrl: 'http://video.example.edu/watch?v=1' }, [cdn]), 'http://video.example.edu/');
  // no tracks known (nothing to compare): as before, the page without fragment
  assert.equal(refererFor({ request: {}, pageUrl: 'https://a.example/p?q=1#f' }), 'https://a.example/p?q=1');
  assert.equal(refererFor({ request: { sendReferer: false }, pageUrl: page }, [same]), '');
  assert.equal(refererFor({ request: {}, pageUrl: 'not a url' }, [same]), '');
});

test('site access the popup offers: the page’s origin; the tracks’ hosts apart, only when cookies are read (security review L4)', () => {
  const t = (id, url, extra = {}) => ({ id, url, kind: 'video', format: 'file', sizeBytes: 1, protected: false, ...extra });
  const page = 'https://video.example.edu/watch/1?x=y';
  const tracks = [t('a', 'https://cdn.example.net/a.mp4'), t('b', 'https://video.example.edu/b.mp4'), t('c', 'https://drm.example.org/c.mpd', { protected: true }), t('d', 'https://rr1.googlevideo.com/x'), t('e', 'http://plain.example.com/e.mp4'), t('f', 'https://cdn.example.net/f.m4a')];
  // generic detection reads the tracks' cookies: their other hosts, each once, sorted
  assert.deepEqual(accessOffer({ status: 'ok', pageUrl: page, request: {}, tracks }), { page: 'https://video.example.edu/*', cookieHosts: ['https://cdn.example.net/*'] });
  // a profile that reads no cookies (Panopto): the page only
  assert.deepEqual(accessOffer({ status: 'ok', pageUrl: page, request: { cookieUrls: [] }, tracks }), { page: 'https://video.example.edu/*', cookieHosts: [] });
  // a plain-HTTP page cannot be granted at all; a result that is not ok neither
  assert.deepEqual(accessOffer({ status: 'ok', pageUrl: 'http://video.example.edu/', request: {}, tracks }), { page: '', cookieHosts: [] });
  assert.deepEqual(accessOffer({ status: 'no_access' }), { page: '', cookieHosts: [] });
  assert.deepEqual(accessOffer(null), { page: '', cookieHosts: [] });
});

test('check items', () => {
  assert.equal(checkOutcome(null), 'new');
  assert.equal(checkOutcome({ state: 'imported' }), 'imported');
  for (const s of ['queued', 'downloading', 'merging', 'importing']) assert.equal(checkOutcome({ state: s }), 'active');
  assert.equal(checkOutcome({ state: 'needs_refresh' }), 'needs_refresh');
  assert.equal(checkOutcome({ state: 'failed' }), 'again');
  assert.equal(checkOutcome({ state: 'cancelled' }), 'again');
});

test('the popup’s view', () => {
  const ok = { status: 'ok', tracks: [V1, A1], downloadPolicy: 'enabled' };
  const conn = { status: 'connected' };
  const v = (o) => popupView({ detection: ok, connection: conn, checked: true, ...o });
  assert.equal(popupView({ detection: null }).view, 'loading');
  assert.equal(popupView({ detection: { status: 'unsupported' } }).view, 'unsupported');
  assert.deepEqual(popupView({ detection: { status: 'no_access' } }), { view: 'none', reason: 'no_access' });
  assert.deepEqual(popupView({ detection: { ...ok, tracks: [] }, connection: conn }), { view: 'none', reason: 'no_tracks' });
  assert.equal(popupView({ detection: ok, connection: { status: 'checking' } }).view, 'loading');
  assert.deepEqual(popupView({ detection: ok, connection: { status: 'unavailable', reason: 'app.not_running' } }), { view: 'disconnected', reason: 'app.not_running' });
  assert.equal(v({ checked: false }).view, 'loading');
  assert.deepEqual(v({}), { view: 'ready', reason: '' });
  assert.deepEqual(v({ checkItem: { state: 'failed' } }), { view: 'ready', reason: 'failed' });
  assert.deepEqual(v({ checkItem: { state: 'imported' } }), { view: 'blocked', reason: 'imported' });
  assert.equal(v({ checkItem: { state: 'downloading' } }).view, 'queued');
  assert.equal(v({ checkItem: { state: 'needs_refresh' } }).view, 'refresh');
  assert.deepEqual(v({ detection: { ...ok, tracks: [P1] } }), { view: 'blocked', reason: 'protected' });
  assert.deepEqual(v({ detection: { ...ok, downloadPolicy: 'disabled' } }), { view: 'blocked', reason: 'disabled' });
  assert.deepEqual(v({ detection: { ...ok, downloadPolicy: 'disabled' }, override: true }), { view: 'ready', reason: '' });
  assert.equal(v({ task: { taskId: 3, state: 'queued' } }).view, 'queued');
  assert.deepEqual(progressOf({ bytesDone: 50, bytesTotal: 200 }), { percent: 25, done: 50, total: 200 });
  assert.deepEqual(progressOf({ bytesDone: 50, bytesTotal: -1 }), { percent: -1, done: 50, total: -1 });
  assert.equal(isFinal('done') && isFinal('needs_refresh') && !isFinal('downloading'), true);
});

test('do-not-handle sites (lead ruling 10): YouTube and its media hosts, subdomains included', () => {
  for (const h of ['youtube.com', 'www.youtube.com', 'm.youtube.com', 'youtu.be', 'www.youtube-nocookie.com', 'rr3---sn-abc.googlevideo.com', 'YOUTUBE.COM.']) assert.equal(isBlockedHost(h), true, h);
  for (const h of ['notyoutube.com', 'youtube.com.example.org', 'video.example.edu', 'tenant.cloud.panopto.eu', '']) assert.equal(isBlockedHost(h), false, h);
  assert.equal(isBlockedUrl('https://www.youtube.com/watch?v=x'), true);
  assert.equal(isBlockedUrl('not a url'), false);
  assert.equal(wildcardTouchesBlocked('*.youtube.com'), true);
  assert.equal(wildcardTouchesBlocked('*.googlevideo.com'), true);
  assert.equal(wildcardTouchesBlocked('music.youtube.com'), true);
  assert.equal(wildcardTouchesBlocked('*.panopto.eu'), false);
});
