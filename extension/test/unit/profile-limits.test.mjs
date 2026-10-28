// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// What a profile (above all a locally imported one) may not do (security
// review L7): wildcards over a whole registry or shared host, regular
// expressions that backtrack badly or run on long inputs, a sessionInfo
// request anywhere but its declared same-origin path, and quietly taking
// over a packaged profile's id.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { checkProfile, wideWildcard, regexComplexity, profileRegex, MAX_REGEX_INPUT, profilePermissionOrigins } from '../../src/lib/profile.js';
import { sessionInfoRequest, sessionInfoSpec } from '../../src/lib/inpage.js';
import { localEntry, activeLocalEntries, confirmedReplacement, withLocalEntry } from '../../src/lib/localprofiles.js';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');
const schema = JSON.parse(fs.readFileSync(path.join(root, 'schema', 'profile.schema.json'), 'utf8'));
const panopto = JSON.parse(fs.readFileSync(path.join(root, 'profiles', 'panopto.json'), 'utf8'));
const clone = (o) => JSON.parse(JSON.stringify(o));
const errorsOf = (mutate) => {
  const p = clone(panopto);
  mutate(p);
  return checkProfile(schema, p);
};

test('wildcards over a country registry or a shared host are refused; the platform’s own domain is fine', () => {
  for (const wide of ['*.co.uk', '*.ac.uk', '*.com.au', '*.edu.au', '*.edu.cn', '*.ac.jp', '*.gov.in', '*.github.io', '*.cloudfront.net', '*.amazonaws.com', '*.s3.amazonaws.com', '*.s3.eu-west-1.amazonaws.com', '*.s3-eu-west-1.amazonaws.com', '*.azurewebsites.net', '*.blob.core.windows.net', '*.akamaized.net', '*.netlify.app']) {
    assert.notEqual(wideWildcard(wide), '', wide);
  }
  for (const fine of ['*.panopto.com', '*.cloud.panopto.eu', '*.ox.ac.uk', '*.unimelb.edu.au', 'd1234abcd.cloudfront.net', '*.d1234abcd.cloudfront.net', 'video.example.co.uk', '*.kaltura.com', '*.mediasite.example.edu', 'github.io']) {
    assert.equal(wideWildcard(fine), '', fine);
  }
  assert.ok(errorsOf((p) => p.match.hosts.push('*.co.uk')).some((e) => e.path === `/match/hosts/${panopto.match.hosts.length}` && /^"\*\.co\.uk" covers a whole public suffix/.test(e.message)));
  assert.ok(errorsOf((p) => (p.match.mediaHosts = ['*.cloudfront.net'])).some((e) => e.path === '/match/mediaHosts/0' && /^"\*\.cloudfront\.net" is a shared service \(cloudfront\.net\)/.test(e.message)), 'the words of scripts/profilecheck');
  // one label after "*." the schema refuses already
  assert.ok(errorsOf((p) => p.match.hosts.push('*.com')).length > 0);
});

test('regular expressions: no backreferences, no repeated group holding a repetition or an alternation, at most three unbounded quantifiers', () => {
  // what the packaged profile uses passes
  for (const src of ['^/Panopto/Pages/(Viewer|Embed)\\.aspx$', '/sessions/[0-9a-f-]{36}/[0-9a-f-]{36}(-[0-9a-f-]{36})?\\.hls/master\\.m3u8$', '^(?!Panopto Viewer$)(.+)$', '([A-Z][a-z]{2} \\d{1,2} \\d{1,2}:\\d{2}(?::\\d{2})? [A-Z]{2,5} \\d{4})\\s*$', '^(?<id>[0-9a-f]{8})$', '[(+*)]+x']) {
    assert.equal(regexComplexity(src), '', src);
  }
  for (const [src, why] of [
    ['(a+)+$', /catastrophic/],
    ['^(\\w+\\s?)*$', /catastrophic/],
    ['(a|aa)*b', /catastrophic/],
    ['(?:x[a-z]*){2,}', /catastrophic/],
    ['((ab)*c)+', /catastrophic/],
    ['(.*a){3}', /catastrophic/],
    ['(a)\\1', /backreferences/],
    ['(?<x>a)\\k<x>', /backreferences/],
    ['.*a.*b.*c.*d', /too many/],
  ]) {
    assert.match(regexComplexity(src), why, src);
  }
  // in a profile, at every regex field
  assert.ok(errorsOf((p) => (p.tracks[1].detect.urlPattern = '(a+)+$')).some((e) => e.path === '/tracks/1/detect/urlPattern' && /catastrophic/.test(e.message)));
  assert.ok(errorsOf((p) => (p.match.pathPattern = '^(/[a-z]+)*$')).some((e) => e.path === '/match/pathPattern'));
  assert.ok(errorsOf((p) => (p.lecture.title[3].pattern = '(.+)+x')).some((e) => e.path === '/lecture/title/3/pattern'));
  assert.ok(errorsOf((p) => (p.inpage.list.idPattern = '^((a|b)+)+$')).some((e) => e.path === '/inpage/list/idPattern'));
  assert.ok(errorsOf((p) => (p.observe[0].urlPattern = 'x'.repeat(301))).some((e) => e.path === '/observe/0/urlPattern'), 'longer than the schema allows');
});

test('a profile regex never runs on more than MAX_REGEX_INPUT characters', () => {
  const re = profileRegex('^/Panopto/.*$');
  assert.equal(re.test('/Panopto/x'), true);
  assert.equal(re.test(`/Panopto/${'x'.repeat(MAX_REGEX_INPUT)}`), false);
  assert.equal(re.exec(`/Panopto/${'x'.repeat(MAX_REGEX_INPUT)}`), null);
  assert.deepEqual([...profileRegex('^/v/(\\d+)$').exec('/v/42')], ['/v/42', '42']);
});

test('sessionInfo: exactly the declared path on the frame’s own origin, on a profile host, matching its observe pattern', () => {
  const spec = sessionInfoSpec(panopto);
  assert.equal(spec.urlPattern, panopto.observe[0].urlPattern);
  assert.deepEqual(spec.hosts, panopto.match.hosts);
  const ID = 'a0000000-0000-4000-8000-0000000000a1';
  const LIST = 'https://tenant.cloud.panopto.eu/Panopto/Pages/Sessions/List.aspx';
  assert.equal(sessionInfoRequest(spec, ID, LIST).url, 'https://tenant.cloud.panopto.eu/Panopto/Pages/Viewer/DeliveryInfo.aspx');
  // a frame on another host (the profile's list path served elsewhere)
  assert.equal(sessionInfoRequest(spec, ID, 'https://lms.example.edu/Panopto/Pages/Sessions/List.aspx'), null);
  // a path that does not match the observe pattern, dot segments, encoded tricks, a query
  for (const p of ['/Panopto/Pages/Viewer/Other.aspx', '/Panopto/../Pages/Viewer/DeliveryInfo.aspx', '/./Panopto/Pages/Viewer/DeliveryInfo.aspx', '/Panopto/Pages/Viewer/DeliveryInfo.aspx?x=1', '//evil.example.com/Panopto/Pages/Viewer/DeliveryInfo.aspx', '/Panopto/Pages/Viewer/DeliveryInfo.aspx#f', '/a b/DeliveryInfo.aspx', '/%2e%2e/Panopto/Pages/Viewer/DeliveryInfo.aspx', 'Panopto/Pages/Viewer/DeliveryInfo.aspx']) {
    assert.equal(sessionInfoRequest({ ...spec, path: p }, ID, LIST), null, p);
  }
  // only GET or POST
  assert.equal(sessionInfoRequest({ ...spec, method: 'PUT' }, ID, LIST), null);
  // the schema says the same about the path
  const errs = (p) => checkProfile(schema, { ...clone(panopto), sessionInfo: { ...panopto.sessionInfo, path: p } });
  assert.deepEqual(errs('/Panopto/Pages/Viewer/DeliveryInfo.aspx'), []);
  for (const p of ['/Panopto/../x/DeliveryInfo.aspx', '/Panopto/Pages/Viewer/DeliveryInfo.aspx?x=1', '//evil.example.com/x', '/a b', '/x\\y']) assert.ok(errs(p).length > 0, p);
});

test('a local file with a packaged profile’s id is used only once the user confirmed it replaces it', () => {
  const mine = { ...clone(panopto), version: '1.2.0-dev' };
  const other = { ...clone(panopto), id: 'echo360', name: 'Echo360' };
  const unconfirmed = localEntry(mine, { now: new Date('2026-10-27T01:00:00Z') });
  assert.deepEqual(Object.keys(unconfirmed).sort(), ['importedAt', 'profile']);
  assert.equal(unconfirmed.importedAt, '2026-10-27');
  assert.equal(confirmedReplacement(unconfirmed), false);
  const confirmed = localEntry(mine, { replaces: '1.1.1' });
  assert.equal(confirmed.replaces, '1.1.1');
  assert.equal(confirmedReplacement(confirmed), true);
  const fresh = localEntry(other);
  // with a packaged "panopto": the unconfirmed one is not in force, the confirmed one is; other ids always are
  assert.deepEqual(activeLocalEntries([unconfirmed, fresh], ['panopto']).map((e) => e.profile.id), ['echo360']);
  assert.deepEqual(activeLocalEntries([confirmed, fresh], ['panopto']).map((e) => e.profile.id), ['panopto', 'echo360']);
  assert.deepEqual(activeLocalEntries([unconfirmed], []).length, 1, 'no packaged profile of that id: nothing to confirm');
  assert.deepEqual(activeLocalEntries([null, {}, { profile: 'x' }, 'x'], ['panopto']), []);
  assert.deepEqual(activeLocalEntries(undefined, ['panopto']), []);
  // importing again replaces the earlier entry of that id
  assert.deepEqual(withLocalEntry([unconfirmed, fresh], confirmed).map((e) => [e.profile.id, !!e.replaces]), [['echo360', false], ['panopto', true]]);
});

test('profile site access: page hosts, and media hosts only when asked for', () => {
  const p = { match: { hosts: ['*.video.example.edu'], mediaHosts: ['media.example.net'] } };
  assert.deepEqual(profilePermissionOrigins(p), ['https://*.video.example.edu/*', 'https://media.example.net/*']);
  assert.deepEqual(profilePermissionOrigins(p, { media: false }), ['https://*.video.example.edu/*']);
});
