// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { normalizePageUrl, genericSessionId, sha256Hex, urlKey, withoutQuery, hostMatches, hostPermissionPattern, originPattern, requestableOrigin, extensionOfPath, potentiallyTrustworthy } from '../../src/lib/url.js';
import { classifyUrl, kindOfCodecs, C_HLS, C_DASH, C_FILE, C_SEGMENT, C_OTHER } from '../../src/lib/classify.js';

test('page address normalisation for the generic session id (ext-brief §3)', () => {
  assert.equal(normalizePageUrl('https://Example.EDU:443/a/b?z=1&a=2&m=3#frag'), 'https://example.edu/a/b?a=2&m=3&z=1');
  // repeated keys keep their order (stable sort)
  assert.equal(normalizePageUrl('https://x.org/p?b=2&a=9&b=1'), 'https://x.org/p?a=9&b=2&b=1');
  assert.equal(normalizePageUrl('https://x.org/p?#'), 'https://x.org/p');
  assert.equal(normalizePageUrl('https://user:pw@x.org/p'), 'https://x.org/p');
  assert.equal(normalizePageUrl('chrome://settings'), null);
  assert.equal(normalizePageUrl('not a url'), null);
});

test('generic session id: first 32 hex digits of SHA-256 of the normalised address', async () => {
  const a = await genericSessionId('https://x.org/watch?b=2&a=1#t=30');
  const b = await genericSessionId('https://X.org/watch?a=1&b=2');
  assert.match(a, /^[0-9a-f]{32}$/);
  assert.equal(a, b);
  assert.equal(a, (await sha256Hex('https://x.org/watch?a=1&b=2')).slice(0, 32));
  assert.notEqual(a, await genericSessionId('https://x.org/watch?a=1&b=3'));
  assert.equal(await genericSessionId('about:blank'), '');
  // the known digest of a fixed string, so the algorithm cannot drift
  assert.equal(await sha256Hex('abc'), 'ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad');
});

test('URL keys drop the fragment only; withoutQuery keeps scheme, host, path', () => {
  assert.equal(urlKey('https://h/x.mp4?sig=1#t=5'), 'https://h/x.mp4?sig=1');
  assert.equal(urlKey('blob:https://h/1'), null);
  assert.equal(withoutQuery('https://u:p@h:8443/a/b.m3u8?x=1#y'), 'https://h:8443/a/b.m3u8');
  assert.equal(extensionOfPath('/a/master.M3U8'), 'm3u8');
  assert.equal(extensionOfPath('/a/noext'), '');
  assert.equal(extensionOfPath('/a/.hidden'), '');
});

test('potentially trustworthy URLs (Secure Contexts): https and loopback hosts only', () => {
  const pt = (raw) => potentiallyTrustworthy(new URL(raw));
  for (const raw of ['https://example.edu/', 'https://10.0.0.1/', 'http://127.0.0.1:8080/x', 'http://127.255.0.1/', 'http://127.1/', 'http://0x7f.0.0.1/', 'http://[::1]/', 'http://[::ffff:127.0.0.1]/', 'http://localhost/', 'http://LOCALHOST./', 'http://a.b.localhost/']) {
    assert.equal(pt(raw), true, raw);
  }
  for (const raw of ['http://example.edu/', 'http://10.0.0.1/', 'http://128.0.0.1/', 'http://[::2]/', 'http://[::ffff:10.0.0.1]/', 'http://localhost.example.edu/', 'http://notlocalhost/', 'ftp://127.0.0.1/']) {
    assert.equal(pt(raw), false, raw);
  }
  assert.equal(potentiallyTrustworthy(null), false);
});

test('host wildcards follow match-pattern semantics', () => {
  assert.equal(hostMatches('*.panopto.eu', 'recap.cloud.panopto.eu'), true);
  assert.equal(hostMatches('*.panopto.eu', 'panopto.eu'), true);
  assert.equal(hostMatches('*.panopto.eu', 'evilpanopto.eu'), false);
  assert.equal(hostMatches('*.panopto.eu', 'panopto.eu.example.com'), false);
  assert.equal(hostMatches('video.example.edu', 'VIDEO.example.edu'), true);
  assert.equal(hostMatches('video.example.edu', 'x.video.example.edu'), false);
  assert.equal(hostPermissionPattern('*.panopto.com'), 'https://*.panopto.com/*');
  assert.equal(originPattern('https://a.b.c:8080/x?y'), 'https://a.b.c/*');
  assert.equal(originPattern('http://a.b.c/x'), 'http://a.b.c/*');
  assert.equal(requestableOrigin('https://a.b.c:8080/x?y'), 'https://a.b.c/*');
  assert.equal(requestableOrigin('http://a.b.c/x'), null); // no plain-HTTP host access is ever requested
});

test('classification by Content-Type, extension and how it was loaded', () => {
  const c = (u, h) => classifyUrl(u, h);
  assert.deepEqual(c('https://h/x', { contentType: 'application/vnd.apple.mpegurl' }), { cls: C_HLS, kind: 'unknown' });
  assert.deepEqual(c('https://h/x', { contentType: 'application/x-mpegURL; charset=utf-8' }), { cls: C_HLS, kind: 'unknown' });
  assert.equal(c('https://h/x', { contentType: 'application/dash+xml' }).cls, C_DASH);
  assert.equal(c('https://h/a/master.m3u8').cls, C_HLS);
  assert.equal(c('https://h/a/stream.mpd?token=1').cls, C_DASH);
  assert.deepEqual(c('https://h/a/talk.m4a'), { cls: C_FILE, kind: 'audio' });
  assert.deepEqual(c('https://h/a/lecture.mp4'), { cls: C_FILE, kind: 'video' });
  // the server's type wins over the name
  assert.deepEqual(c('https://h/a/lecture.mp4', { contentType: 'audio/mp4' }), { cls: C_FILE, kind: 'audio' });
  assert.deepEqual(c('https://h/stream', { contentType: 'video/webm' }), { cls: C_FILE, kind: 'video' });
  // an HTML error page with a media name is not media
  assert.equal(c('https://h/a/lecture.mp4', { contentType: 'text/html' }).cls, C_OTHER);
  assert.equal(c('https://h/a/lecture.mp4', { status: 403 }).cls, C_OTHER);
  // ambiguous types fall back to the name
  assert.deepEqual(c('https://h/a/lecture.mp4', { contentType: 'application/octet-stream' }), { cls: C_FILE, kind: 'video' });
  // segments are never tracks
  assert.equal(c('https://h/a/seg_001.ts').cls, C_SEGMENT);
  assert.equal(c('https://h/a/chunk-1.m4s').cls, C_SEGMENT);
  assert.equal(c('https://h/x', { contentType: 'video/MP2T' }).cls, C_SEGMENT);
  // Azure Media Services style paths
  assert.equal(c('https://h/v.ism/manifest(format=m3u8-aapl)').cls, C_HLS);
  assert.equal(c('https://h/v.ism/manifest(format=mpd-time-csf)').cls, C_DASH);
  assert.equal(c('https://h/v.ism/manifest').cls, C_SEGMENT);
  // without a name or type, the media element or request type decides
  assert.deepEqual(c('https://h/play?id=1', { tag: 'audio' }), { cls: C_FILE, kind: 'audio' });
  assert.deepEqual(c('https://h/play?id=1', { resourceType: 'media' }), { cls: C_FILE, kind: 'unknown' });
  assert.equal(c('https://h/app.js').cls, C_OTHER);
  assert.equal(c('blob:https://h/1').cls, C_OTHER);
});

test('codec lists', () => {
  assert.equal(kindOfCodecs('mp4a.40.2'), 'audio');
  assert.equal(kindOfCodecs('avc1.4d401f,mp4a.40.2'), 'video');
  assert.equal(kindOfCodecs('ec-3, ac-3'), 'audio');
  assert.equal(kindOfCodecs(''), 'unknown');
  assert.equal(kindOfCodecs('stpp'), 'unknown');
});
