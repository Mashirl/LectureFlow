// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Pinned downloads for the build: every file is fetched once into the cache
// (desktop/.cache/downloads, git-ignored), checked against its SHA-256, and
// only then used. A mismatch fails the build.
'use strict';

const crypto = require('node:crypto');
const fs = require('node:fs');
const path = require('node:path');
const { execFileSync } = require('node:child_process');
const { Readable } = require('node:stream');
const { pipeline } = require('node:stream/promises');

function sha256File(file) {
  const h = crypto.createHash('sha256');
  const fd = fs.openSync(file, 'r');
  try {
    const buf = Buffer.alloc(1 << 20);
    let n;
    while ((n = fs.readSync(fd, buf, 0, buf.length, null)) > 0) h.update(buf.subarray(0, n));
  } finally {
    fs.closeSync(fd);
  }
  return h.digest('hex');
}

/** cacheName: a stable file name for a URL (its last segment, made safe). */
function cacheName(url) {
  const last = decodeURIComponent(new URL(url).pathname.split('/').pop() || 'download');
  return last.replace(/[^A-Za-z0-9._-]/g, '_');
}

async function downloadOnce(url, dest, fetchImpl) {
  const res = await fetchImpl(url, { redirect: 'follow' });
  if (!res.ok || !res.body) throw new Error(`download ${url}: HTTP ${res.status}`);
  await pipeline(Readable.fromWeb(res.body), fs.createWriteStream(dest));
}

// curl: the system's (Windows 10+, macOS, Linux runners), for the hosts that
// Node's fetch cannot reach from some CI runners (gmplib.org and ftp.gnu.org
// failed with "fetch failed" on GitHub's Windows runners, 2026-10-28, while
// the same URLs worked elsewhere).
function curlDownload(url, dest) {
  const curl = process.platform === 'win32' ? path.join(process.env.SystemRoot || 'C:\\Windows', 'System32', 'curl.exe') : 'curl';
  execFileSync(curl, ['-fsSL', '--proto', '=https', '--proto-redir', '=https', '--retry', '5', '--retry-all-errors', '--connect-timeout', '30', '-o', dest, url], { stdio: 'inherit' });
}

function why(err) {
  const c = err && err.cause;
  return [err && err.message, c && (c.code || c.message)].filter(Boolean).join(': ');
}

/**
 * download(url, dest): Node's fetch, three tries; with the real fetch, curl
 * after that. Tests pass their own fetchImpl and never reach the network.
 */
async function download(url, dest, fetchImpl = fetch, { log = console.log } = {}) {
  let last;
  for (let i = 0; i < 3; i++) {
    try {
      await downloadOnce(url, dest, fetchImpl);
      return;
    } catch (err) {
      last = err;
      fs.rmSync(dest, { force: true });
      log(`download ${url} failed (${why(err)}), try ${i + 1} of 3`);
      if (i < 2) await new Promise((r) => setTimeout(r, 2000 * (i + 1)));
    }
  }
  if (fetchImpl !== globalThis.fetch) throw last;
  log(`downloading ${url} with curl`);
  try {
    curlDownload(url, dest);
  } catch (err) {
    fs.rmSync(dest, { force: true });
    throw new Error(`download ${url}: fetch (${why(last)}) and curl (${why(err)}) both failed`);
  }
}

/**
 * fetchPinned({ url, sha256 }, cacheDir) → path of the verified file.
 * A cached file with the right hash is reused; anything else is fetched
 * again. The file only gets its final name after the hash matched.
 */
async function fetchPinned({ url, sha256 }, cacheDir, { fetchImpl = fetch, log = console.log } = {}) {
  if (!/^https:\/\//.test(url)) throw new Error(`refusing a non-HTTPS download: ${url}`);
  if (!/^[0-9a-f]{64}$/.test(sha256)) throw new Error(`bad sha256 for ${url}`);
  fs.mkdirSync(cacheDir, { recursive: true });
  const file = path.join(cacheDir, `${sha256.slice(0, 12)}-${cacheName(url)}`);
  if (fs.existsSync(file)) {
    if (sha256File(file) === sha256) return file;
    fs.rmSync(file, { force: true });
  }
  const part = `${file}.part`;
  log(`downloading ${url}`);
  await download(url, part, fetchImpl, { log });
  const got = sha256File(part);
  if (got !== sha256) {
    fs.rmSync(part, { force: true });
    throw new Error(`SHA-256 mismatch for ${url}: expected ${sha256}, got ${got}`);
  }
  fs.renameSync(part, file);
  return file;
}

/** extract(archive, dir): zip or tar.* with the system's bsdtar (Windows 10+, macOS). */
function extract(archive, dir) {
  fs.mkdirSync(dir, { recursive: true });
  const tar = process.platform === 'win32' ? path.join(process.env.SystemRoot || 'C:\\Windows', 'System32', 'tar.exe') : 'tar';
  execFileSync(tar, ['-xf', archive, '-C', dir], { stdio: 'inherit' });
}

/** globFiles(dir, pattern) → files in dir matching "name", "sub/name" or "sub/*.ext". */
function globFiles(dir, pattern) {
  const parts = pattern.split('/');
  const name = parts.pop();
  const base = path.join(dir, ...parts);
  if (!name.includes('*')) {
    const f = path.join(base, name);
    return fs.existsSync(f) ? [f] : [];
  }
  const re = new RegExp(`^${name.replace(/[.+^${}()|[\]\\]/g, '\\$&').replace(/\*/g, '.*')}$`);
  if (!fs.existsSync(base)) return [];
  return fs.readdirSync(base).filter((f) => re.test(f)).map((f) => path.join(base, f));
}

module.exports = { sha256File, cacheName, fetchPinned, extract, globFiles };
