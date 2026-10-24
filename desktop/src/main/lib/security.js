// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Security decisions for the window (SPEC-v3 V1.2, V2). Pure functions: every
// decision takes the URL and the service port and returns a verdict, so the
// rules are unit-tested without Electron.
'use strict';

const { pathToFileURL } = require('node:url');

// The same policy the Go service sends (v3-brief §2). The shell adds it to
// every response from the service as a second line of defence; when both are
// present the browser enforces both.
const CSP =
  "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
  "img-src 'self' data: blob:; media-src 'self' blob:; font-src 'self' data:; " +
  "connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'";

const TOKEN_HEADER = 'X-LF-Token';

// Permissions the app page may use: system notifications (web/app.js),
// fullscreen video (player) and writing to the clipboard (copy buttons).
// Everything else (camera, microphone, geolocation, HID, serial, MIDI,
// external protocols, clipboard read, ...) is denied.
const ALLOWED_PERMISSIONS = new Set(['notifications', 'fullscreen', 'clipboard-sanitized-write']);

function isValidPort(port) {
  return Number.isInteger(port) && port > 0 && port < 65536;
}

function serverOrigin(port) {
  if (!isValidPort(port)) throw new RangeError(`invalid port: ${port}`);
  return `http://127.0.0.1:${port}`;
}

function parse(url) {
  if (typeof url !== 'string' || url.length > 8192) return null;
  try {
    return new URL(url);
  } catch {
    return null;
  }
}

/** isServerUrl: the URL belongs to the local service (exact origin, no credentials). */
function isServerUrl(url, port) {
  if (!isValidPort(port)) return false;
  const u = parse(url);
  if (!u) return false;
  return u.protocol === 'http:' && u.origin === serverOrigin(port) && u.username === '' && u.password === '';
}

/**
 * isExternalWebUrl: a URL that is safe to hand to the system: http and
 * https (browser) with a host and no credentials, or mailto (mail program)
 * with an address. Every other scheme (file:, javascript:, ms-settings:,
 * smb:, custom app protocols) is refused.
 */
function isExternalWebUrl(url) {
  const u = parse(url);
  if (!u) return false;
  if (u.protocol === 'mailto:') return u.pathname.length > 0 && !/[\r\n]/.test(url);
  if (u.protocol !== 'http:' && u.protocol !== 'https:') return false;
  if (u.hostname === '' || u.username !== '' || u.password !== '') return false;
  return true;
}

/**
 * classifyNavigation decides what happens when the page navigates (or opens a
 * window) to url: 'allow' (the local service), 'external' (http/https/mailto:
 * handed to the system, never opened in the app window) or 'deny'.
 */
function classifyNavigation(url, port) {
  if (isServerUrl(url, port)) return 'allow';
  if (isExternalWebUrl(url)) return 'external';
  return 'deny';
}

/** shouldInjectToken: the access token goes only to the local service. */
function shouldInjectToken(url, port) {
  return isServerUrl(url, port);
}

/**
 * isRequestAllowed: network requests the window may make. The local service,
 * in-memory data/blob URLs, the shell's own files (splash page) and, in
 * development, DevTools. No remote resources (SPEC-v3 V2).
 */
function isRequestAllowed(url, port, { shellRoot, devTools = false } = {}) {
  const u = parse(url);
  if (!u) return false;
  switch (u.protocol) {
    case 'http:':
      return isServerUrl(url, port);
    case 'data:':
    case 'blob:':
      return true;
    case 'file:':
      return typeof shellRoot === 'string' && shellRoot !== '' && isUnder(u, shellRoot);
    case 'devtools:':
      return devTools;
    default:
      return false;
  }
}

function isUnder(fileUrl, root) {
  let base = pathToFileURL(root).href;
  if (!base.endsWith('/')) base += '/';
  const href = fileUrl.href.split(/[?#]/)[0];
  return href.toLowerCase().startsWith(base.toLowerCase()) && !/(^|\/)\.\.(\/|$)/.test(href.slice(base.length));
}

/** isPermissionAllowed: permission requests and checks from the window. */
function isPermissionAllowed(permission, requestingUrl, port) {
  if (!ALLOWED_PERMISSIONS.has(permission)) return false;
  return isServerUrl(requestingUrl, port);
}

/** isDownloadAllowed: downloads started by the page (exports). */
function isDownloadAllowed(url, port) {
  const u = parse(url);
  if (!u) return false;
  if (u.protocol === 'data:') return true;
  if (u.protocol === 'blob:') {
    // blob:http://127.0.0.1:PORT/uuid — the origin is embedded.
    return isServerUrl(url.slice('blob:'.length), port);
  }
  return isServerUrl(url, port);
}

/**
 * withSecurityHeaders returns a copy of Electron's responseHeaders
 * (Record<string, string[]>) with the CSP added (kept alongside any policy
 * the service sent; identical values are not duplicated) and nosniff set.
 */
function withSecurityHeaders(responseHeaders) {
  const out = {};
  let cspKey = null;
  let nosniff = false;
  for (const [k, v] of Object.entries(responseHeaders || {})) {
    const values = Array.isArray(v) ? v.slice() : [String(v)];
    out[k] = values;
    const lk = k.toLowerCase();
    if (lk === 'content-security-policy') cspKey = k;
    if (lk === 'x-content-type-options') nosniff = true;
  }
  if (cspKey === null) {
    out['Content-Security-Policy'] = [CSP];
  } else if (!out[cspKey].some((x) => normalizeCsp(x) === normalizeCsp(CSP))) {
    out[cspKey].push(CSP);
  }
  if (!nosniff) out['X-Content-Type-Options'] = ['nosniff'];
  return out;
}

function normalizeCsp(s) {
  return String(s).trim().replace(/\s+/g, ' ').replace(/;\s*$/, '');
}

/** isShellFileUrl: url is one of the shell's own pages (splash) under shellRoot. */
function isShellFileUrl(url, shellRoot) {
  const u = parse(url);
  if (!u || u.protocol !== 'file:') return false;
  return isUnder(u, shellRoot);
}

module.exports = {
  CSP,
  TOKEN_HEADER,
  ALLOWED_PERMISSIONS,
  isValidPort,
  serverOrigin,
  isServerUrl,
  isExternalWebUrl,
  classifyNavigation,
  shouldInjectToken,
  isRequestAllowed,
  isPermissionAllowed,
  isDownloadAllowed,
  withSecurityHeaders,
  isShellFileUrl,
};
