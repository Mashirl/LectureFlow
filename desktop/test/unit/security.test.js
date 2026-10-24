// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const path = require('node:path');
const { pathToFileURL } = require('node:url');

const sec = require('../../src/main/lib/security');

const PORT = 51234;
const O = `http://127.0.0.1:${PORT}`;

test('navigation: only the local service stays in the window', () => {
  const cases = [
    [`${O}/`, 'allow'],
    [`${O}/#/lecture/42`, 'allow'],
    [`${O}/api/health?x=1`, 'allow'],
    [`http://127.0.0.1:${PORT + 1}/`, 'external'], // another local port: never inside the app window
    [`http://localhost:${PORT}/`, 'external'],
    [`https://127.0.0.1:${PORT}/`, 'external'],
    ['https://example.com/docs', 'external'],
    ['http://example.com/', 'external'],
    [`http://user:pw@127.0.0.1:${PORT}/`, 'deny'],
    ['https://user@example.com/', 'deny'],
    [`http://127.0.0.1:${PORT}.evil.com/`, 'deny'], // not even a valid URL
    ['javascript:alert(1)', 'deny'],
    ['file:///C:/Windows/win.ini', 'deny'],
    ['data:text/html,<script>1</script>', 'deny'],
    ['mailto:someone@example.com', 'external'],
    ['mailto:someone@example.com?subject=Hi%20there', 'external'],
    ['mailto:', 'deny'],
    ['ftp://example.com/', 'deny'],
    ['about:blank', 'deny'],
    ['chrome://gpu', 'deny'],
    ['not a url', 'deny'],
    ['', 'deny'],
    [null, 'deny'],
  ];
  for (const [url, want] of cases) assert.equal(sec.classifyNavigation(url, PORT), want, String(url));
});

test('navigation: nothing is allowed while no service port is known', () => {
  assert.equal(sec.classifyNavigation(`${O}/`, null), 'external');
  assert.equal(sec.isServerUrl(`${O}/`, null), false);
  assert.equal(sec.isServerUrl(`${O}/`, 0), false);
  assert.equal(sec.isServerUrl(`${O}/`, 70000), false);
});

test('token goes to the exact service origin only', () => {
  assert.equal(sec.shouldInjectToken(`${O}/api/health`, PORT), true);
  assert.equal(sec.shouldInjectToken(`${O}/api/events`, PORT), true);
  assert.equal(sec.shouldInjectToken(`${O}/media/1.mp4`, PORT), true);
  for (const url of [
    `http://127.0.0.1:${PORT + 1}/api/health`,
    `http://localhost:${PORT}/api/health`,
    `https://127.0.0.1:${PORT}/api/health`,
    `ws://127.0.0.1:${PORT}/api/health`,
    `http://127.0.0.1:${PORT}@evil.com/api/health`,
    `http://evil.com/?u=${O}/api`,
    `http://127.0.0.1.evil.com:${PORT}/api/health`,
    `http://x:y@127.0.0.1:${PORT}/api/health`,
    'https://example.com/',
    'blob:http://127.0.0.1:51234/abc',
  ]) {
    assert.equal(sec.shouldInjectToken(url, PORT), false, url);
  }
});

test('requests: no remote resources', () => {
  const root = path.join(__dirname, '..', '..', 'src');
  const splash = pathToFileURL(path.join(root, 'splash.html')).href;
  const opts = { shellRoot: root, devTools: false };
  assert.equal(sec.isRequestAllowed(`${O}/style.css`, PORT, opts), true);
  assert.equal(sec.isRequestAllowed('data:image/png;base64,AAAA', PORT, opts), true);
  assert.equal(sec.isRequestAllowed(`blob:${O}/x`, PORT, opts), true);
  assert.equal(sec.isRequestAllowed(`${splash}?state=error`, PORT, opts), true);
  assert.equal(sec.isRequestAllowed(pathToFileURL(path.join(root, 'splash.js')).href, PORT, opts), true);
  assert.equal(sec.isRequestAllowed(pathToFileURL(path.join(root, '..', 'package.json')).href, PORT, opts), false);
  assert.equal(sec.isRequestAllowed('file:///C:/Windows/win.ini', PORT, opts), false);
  assert.equal(sec.isRequestAllowed('https://fonts.googleapis.com/css', PORT, opts), false);
  assert.equal(sec.isRequestAllowed('http://example.com/x.png', PORT, opts), false);
  assert.equal(sec.isRequestAllowed(`ws://127.0.0.1:${PORT}/`, PORT, opts), false);
  assert.equal(sec.isRequestAllowed(`http://127.0.0.1:${PORT + 1}/`, PORT, opts), false);
  assert.equal(sec.isRequestAllowed('devtools://devtools/bundled/inspector.html', PORT, opts), false);
  assert.equal(sec.isRequestAllowed('devtools://devtools/bundled/inspector.html', PORT, { ...opts, devTools: true }), true);
  assert.equal(sec.isRequestAllowed('chrome-extension://abc/x.js', PORT, opts), false);
});

test('permissions: notifications, fullscreen and clipboard writing for the service page only', () => {
  for (const p of ['notifications', 'fullscreen', 'clipboard-sanitized-write']) {
    assert.equal(sec.isPermissionAllowed(p, `${O}/`, PORT), true, p);
    assert.equal(sec.isPermissionAllowed(p, O, PORT), true, `${p} origin form`);
    assert.equal(sec.isPermissionAllowed(p, 'https://example.com/', PORT), false, `${p} remote`);
    assert.equal(sec.isPermissionAllowed(p, 'file:///C:/x.html', PORT), false, `${p} file`);
  }
  for (const p of ['media', 'geolocation', 'clipboard-read', 'openExternal', 'hid', 'serial', 'midi', 'pointerLock', 'display-capture', 'unknown']) {
    assert.equal(sec.isPermissionAllowed(p, `${O}/`, PORT), false, p);
  }
});

test('downloads: from the service, data and the service\'s blobs', () => {
  assert.equal(sec.isDownloadAllowed(`${O}/api/lectures/1/export?format=md`, PORT), true);
  assert.equal(sec.isDownloadAllowed(`blob:${O}/0b1c`, PORT), true);
  assert.equal(sec.isDownloadAllowed('data:text/plain,hi', PORT), true);
  assert.equal(sec.isDownloadAllowed('blob:https://example.com/0b1c', PORT), false);
  assert.equal(sec.isDownloadAllowed('https://example.com/setup.exe', PORT), false);
});

test('security headers: CSP added beside the service\'s, nosniff set, nothing removed', () => {
  const plain = sec.withSecurityHeaders({ 'content-type': ['text/html'] });
  assert.deepEqual(plain['Content-Security-Policy'], [sec.CSP]);
  assert.deepEqual(plain['X-Content-Type-Options'], ['nosniff']);
  assert.deepEqual(plain['content-type'], ['text/html']);

  const same = sec.withSecurityHeaders({ 'Content-Security-Policy': [sec.CSP], 'X-Content-Type-Options': ['nosniff'] });
  assert.deepEqual(same['Content-Security-Policy'], [sec.CSP], 'identical policy is not duplicated');
  assert.equal(Object.keys(same).filter((k) => k.toLowerCase() === 'x-content-type-options').length, 1);

  const looser = sec.withSecurityHeaders({ 'content-security-policy': ["default-src *"] });
  assert.deepEqual(looser['content-security-policy'], ['default-src *', sec.CSP], 'both enforced');
  assert.equal(looser['Content-Security-Policy'], undefined);
});

test('the CSP matches the Go service exactly (v3-brief §2)', () => {
  const want = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; media-src 'self' blob:; font-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'";
  assert.equal(sec.CSP, want);
  assert.doesNotMatch(sec.CSP, /unsafe-eval/);
});

test('shell pages: only files under the shell root', () => {
  const root = path.join(__dirname, '..', '..', 'src');
  assert.equal(sec.isShellFileUrl(`${pathToFileURL(path.join(root, 'splash.html')).href}?state=x`, root), true);
  assert.equal(sec.isShellFileUrl(pathToFileURL(path.join(root, '..', 'locales', 'en.json')).href, root), false);
  assert.equal(sec.isShellFileUrl(`${O}/`, root), false);
});
