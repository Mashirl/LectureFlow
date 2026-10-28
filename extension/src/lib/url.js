// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// URL helpers shared by detection, profiles and the report. Pure: no
// browser APIs beyond URL, URLSearchParams and Web Crypto, which Node has too.

/** Parses an absolute http(s) URL; returns null for anything else. */
export function parseHttpUrl(raw, base) {
  let u;
  try {
    u = base === undefined ? new URL(raw) : new URL(raw, base);
  } catch {
    return null;
  }
  if (u.protocol !== 'http:' && u.protocol !== 'https:') return null;
  return u;
}

/**
 * The Secure Contexts standard's "potentially trustworthy URL" for an
 * http(s) URL object: https, or a loopback host (127.0.0.0/8, ::1, also
 * IPv4-mapped, localhost and *.localhost). Browsers send a Referer to such
 * URLs from an HTTPS page; LectureFlow's internal/fetch trustworthy() is the
 * same rule.
 */
export function potentiallyTrustworthy(u) {
  if (!u) return false;
  if (u.protocol === 'https:') return true;
  if (u.protocol !== 'http:') return false;
  const h = u.hostname.toLowerCase().replace(/\.$/, '');
  if (/^\d{1,3}(\.\d{1,3}){3}$/.test(h)) return h.startsWith('127.');
  if (h.startsWith('[') && h.endsWith(']')) {
    // the URL parser has already put IPv6 in its canonical form
    const v6 = h.slice(1, -1);
    return v6 === '::1' || /^::ffff:7f[0-9a-f]{2}:[0-9a-f]{1,4}$/.test(v6);
  }
  return h === 'localhost' || h.endsWith('.localhost');
}

/** The dedup key of a media URL: the absolute URL without its fragment. */
export function urlKey(raw) {
  const u = parseHttpUrl(raw);
  if (!u) return null;
  u.hash = '';
  return u.href;
}

/** scheme://host/path — no user info, query or fragment. */
export function withoutQuery(raw) {
  const u = parseHttpUrl(raw);
  if (!u) return null;
  return `${u.protocol}//${u.host}${u.pathname}`;
}

/** Lower-cased file extension of the URL path ("" when none). */
export function pathExtension(raw) {
  const u = parseHttpUrl(raw);
  if (!u) return '';
  return extensionOfPath(u.pathname);
}

export function extensionOfPath(pathname) {
  const last = pathname.slice(pathname.lastIndexOf('/') + 1);
  const dot = last.lastIndexOf('.');
  if (dot <= 0 || dot === last.length - 1) return '';
  const ext = last.slice(dot + 1).toLowerCase();
  return /^[a-z0-9]{1,6}$/.test(ext) ? ext : '';
}

/**
 * Host wildcard with the semantics of a WebExtension match pattern: "*"
 * matches every host, "*.example.com" matches example.com and every
 * subdomain, anything else matches exactly. Case-insensitive.
 */
export function hostMatches(pattern, host) {
  const p = String(pattern).toLowerCase();
  const h = String(host).toLowerCase().replace(/\.$/, '');
  if (p === '*') return true;
  if (p.startsWith('*.')) {
    const base = p.slice(2);
    return h === base || h.endsWith('.' + base);
  }
  return h === p;
}

/**
 * The optional-permission origin pattern for a host wildcard. HTTPS only:
 * the manifest offers no plain-HTTP host access.
 */
export function hostPermissionPattern(hostPattern) {
  return `https://${String(hostPattern).toLowerCase()}/*`;
}

/** Origin pattern ("https://host/*") of an absolute URL, for permission checks. */
export function originPattern(raw) {
  const u = parseHttpUrl(raw);
  if (!u) return null;
  return `${u.protocol}//${u.hostname}/*`;
}

/** The pattern the user can be asked to grant for a URL: HTTPS only. */
export function requestableOrigin(raw) {
  const u = parseHttpUrl(raw);
  if (!u || u.protocol !== 'https:') return null;
  return `https://${u.hostname}/*`;
}

/**
 * Page address normalised for the generic session id (ext-brief §3):
 * fragment dropped, query parameters sorted by key (stable for repeated
 * keys), host lower-cased and default port dropped by the URL parser.
 */
export function normalizePageUrl(raw) {
  const u = parseHttpUrl(raw);
  if (!u) return null;
  u.hash = '';
  u.username = '';
  u.password = '';
  const params = new URLSearchParams(u.search);
  params.sort();
  const q = params.toString();
  u.search = q ? '?' + q : '';
  return u.href;
}

/** Hex SHA-256 of a UTF-8 string (Web Crypto). */
export async function sha256Hex(text) {
  const data = new TextEncoder().encode(text);
  const digest = await globalThis.crypto.subtle.digest('SHA-256', data);
  return Array.from(new Uint8Array(digest), (b) => b.toString(16).padStart(2, '0')).join('');
}

/**
 * Session id of a page when no profile matched: the first 32 hex digits of
 * the SHA-256 of the normalised page address. The address itself is never
 * sent to LectureFlow.
 */
export async function genericSessionId(pageUrl) {
  const norm = normalizePageUrl(pageUrl);
  if (!norm) return '';
  return (await sha256Hex(norm)).slice(0, 32);
}
