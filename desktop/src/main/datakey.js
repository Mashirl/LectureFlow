// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The data key of the stored credentials on macOS (cred-enc, user decision
// 2026-10-27). The service seals the providers' API keys and the Colab
// authorisation with a 32-byte data key (internal/secrets). On Windows the
// service protects it itself with DPAPI; on macOS Go cannot reach the
// Keychain without cgo, so the shell keeps the key here: encrypted with
// Electron's safeStorage (the Keychain item "LectureFlow Safe Storage") in
// the profile (userData/datakey.bin), and handed to the service on stdin
// right after the access token (server.js, -datakey-stdin).
//
// A datakey.bin that cannot be decrypted is never overwritten. When the
// Keychain itself does not work now (access refused, locked: a probe value
// does not round-trip either), nothing is touched and UNAVAILABLE is
// returned: the service runs this time without a key (the saved
// credentials ask to be entered again) and the next start tries again
// (sec3 L4). Only when the Keychain works and the file still does not open
// (another Mac or account, a reset Keychain) is it renamed to
// datakey.bin.unreadable-<time> and a new key made. Nothing here logs the
// key.
'use strict';

const crypto = require('node:crypto');
const fs = require('node:fs');
const path = require('node:path');

const KEY_FILE = 'datakey.bin';
const KEY_BYTES = 32;
const HEX_RE = /^[0-9a-f]{64}$/;

const noLog = { info() {}, warn() {}, error() {} };

function stamp(d) {
  const p = (n) => String(n).padStart(2, '0');
  return `${d.getFullYear()}${p(d.getMonth() + 1)}${p(d.getDate())}-${p(d.getHours())}${p(d.getMinutes())}${p(d.getSeconds())}`;
}

/** writeAtomic writes data to file through a temporary file (0600) and a rename. */
function writeAtomic(fsMod, file, data) {
  const tmp = `${file}.tmp-${process.pid}-${crypto.randomBytes(4).toString('hex')}`;
  try {
    fsMod.writeFileSync(tmp, data, { mode: 0o600 });
    fsMod.renameSync(tmp, file);
  } catch (err) {
    try {
      fsMod.rmSync(tmp, { force: true });
    } catch {
      // nothing left to clean up
    }
    throw err;
  }
}

/** UNAVAILABLE: the Keychain cannot be used now; the service gets no key this run. */
const UNAVAILABLE = Object.freeze({ unavailable: true });

/** keychainWorks: a probe value round-trips through safeStorage now. */
function keychainWorks(safeStorage) {
  try {
    const probe = 'lectureflow-probe';
    return safeStorage.decryptString(safeStorage.encryptString(probe)) === probe;
  } catch {
    return false;
  }
}

/**
 * loadOrCreateDataKey → Buffer (32 bytes) | UNAVAILABLE | null.
 *   safeStorage   Electron's safeStorage (tests: a fake); none → null
 *   dir           the profile folder (userData)
 *   fsMod, randomBytes, now, log   injectable for tests
 */
function loadOrCreateDataKey({ safeStorage, dir, fsMod = fs, randomBytes = crypto.randomBytes, now = () => new Date(), log = noLog }) {
  if (!safeStorage || !dir) return null;
  let available = false;
  try {
    available = typeof safeStorage.isEncryptionAvailable === 'function' && safeStorage.isEncryptionAvailable();
  } catch {
    available = false;
  }
  if (!available) {
    log.warn('datakey: the Keychain is not available now; the service runs without the credentials key this time');
    return UNAVAILABLE;
  }
  const file = path.join(dir, KEY_FILE);
  let blob = null;
  try {
    blob = fsMod.readFileSync(file);
  } catch (err) {
    if (err.code !== 'ENOENT') {
      // not readable now (permissions, I/O): never replaced for that
      log.warn(`datakey: ${KEY_FILE} cannot be read (${err.code || 'error'}); no key this time`);
      return UNAVAILABLE;
    }
  }
  if (blob) {
    let hex = null;
    let threw = false;
    try {
      hex = safeStorage.decryptString(blob);
    } catch {
      threw = true;
    }
    if (!threw && typeof hex === 'string' && HEX_RE.test(hex)) return Buffer.from(hex, 'hex');
    if (threw && !keychainWorks(safeStorage)) {
      // refused or locked: the file stays, the next start tries again
      log.warn(`datakey: the Keychain refused or could not decrypt now; ${KEY_FILE} is left as it is and no key is used this time`);
      return UNAVAILABLE;
    }
    const kept = `${file}.unreadable-${stamp(now())}`;
    try {
      fsMod.renameSync(file, kept);
    } catch (err) {
      log.error(`datakey: ${KEY_FILE} cannot be decrypted and could not be kept aside (${err.code || 'error'}); no key this time`);
      return UNAVAILABLE;
    }
    log.warn(`datakey: ${KEY_FILE} cannot be decrypted on this Mac or account; it was kept as ${path.basename(kept)} and a new key made (saved API keys must be entered again)`);
  }
  const key = randomBytes(KEY_BYTES);
  let enc;
  try {
    enc = safeStorage.encryptString(key.toString('hex'));
  } catch {
    log.warn('datakey: the Keychain refused to encrypt a new key; no key this time');
    return UNAVAILABLE;
  }
  try {
    writeAtomic(fsMod, file, enc);
  } catch (err) {
    log.error(`datakey: the new key could not be saved (${err.code || err.message}); no key this time`);
    return UNAVAILABLE;
  }
  log.info('datakey: a new credentials key was made (macOS Keychain)');
  return key;
}

/**
 * dataKeyForService → Buffer | UNAVAILABLE | null: what the service gets on
 * stdin (null: nothing, the service keeps its own key).
 * Only on macOS (Windows: the service uses DPAPI itself). electron is
 * injectable for tests; otherwise it is required here, lazily, so that
 * server.js stays free of Electron imports.
 */
function dataKeyForService({ platform = process.platform, electron = null, log = noLog } = {}) {
  if (platform !== 'darwin') return null;
  let e = electron;
  if (!e) {
    try {
      e = require('electron');
    } catch {
      return null;
    }
  }
  // plain Node (unit tests) gets the binary's path, not the API
  if (!e || typeof e !== 'object' || !e.app || !e.safeStorage) return null;
  let dir;
  try {
    dir = e.app.getPath('userData');
  } catch {
    return null;
  }
  return loadOrCreateDataKey({ safeStorage: e.safeStorage, dir, log });
}

module.exports = { loadOrCreateDataKey, dataKeyForService, UNAVAILABLE, KEY_FILE, KEY_BYTES };
