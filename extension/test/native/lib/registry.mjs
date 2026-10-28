// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The one registry value the native end-to-end test writes (the user agreed
// to exactly this one, 2026-10-27): HKCU\Software\Chromium\NativeMessagingHosts
// \org.lectureflow.nmhost, which Playwright's Chrome for Testing reads first
// (its chrome.dll looks up SOFTWARE\Chromium\NativeMessagingHosts before
// SOFTWARE\Google\Chrome\NativeMessagingHosts). It is created only when it
// does not exist, removed at the end (also after a failure), and nothing
// else under Software\Google, Software\Microsoft or Software\Mozilla is ever
// touched. A value left by an earlier run of this test (it names a manifest
// in a "lf-native-e2e" directory) is removed first; any other existing value
// stops the test.

import { execFileSync } from 'node:child_process';

export const HOST_KEY = 'HKCU\\Software\\Chromium\\NativeMessagingHosts\\org.lectureflow.nmhost';
export const MARKER = 'lf-native-e2e';

function reg(args) {
  try {
    return { code: 0, out: execFileSync('reg.exe', args, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'], windowsHide: true }) };
  } catch (e) {
    return { code: e.status ?? 1, out: String(e.stdout || '') + String(e.stderr || '') };
  }
}

/** The key's default value, or null when the key does not exist. */
export function readHostKey() {
  const r = reg(['query', HOST_KEY, '/ve']);
  if (r.code !== 0) return null;
  const m = /\(Default\)\s+REG_SZ\s+(.*)/i.exec(r.out) || /REG_SZ\s+(.*)/i.exec(r.out);
  return m ? m[1].trim() : '';
}

let created = false;

/** Points the key at the manifest; throws when someone else's value is there. */
export function createHostKey(manifestPath) {
  const existing = readHostKey();
  if (existing !== null) {
    if (!existing.includes(MARKER)) {
      throw new Error(`${HOST_KEY} already exists (${existing}); it is not this test's, so the test stops without touching it`);
    }
    console.log(`# removing a value an earlier native e2e run left behind: ${existing}`);
    reg(['delete', HOST_KEY, '/f']);
  }
  const r = reg(['add', HOST_KEY, '/ve', '/t', 'REG_SZ', '/d', manifestPath, '/f']);
  if (r.code !== 0) throw new Error(`reg add failed: ${r.out.trim()}`);
  created = true;
  if (readHostKey() !== manifestPath) throw new Error('the registry value did not stick');
}

/** Removes the key if this process created it; returns the query output after removal. */
export function deleteHostKey() {
  if (created) {
    const value = readHostKey();
    if (value !== null && value.includes(MARKER)) reg(['delete', HOST_KEY, '/f']);
    created = false;
  }
  return reg(['query', HOST_KEY]).out.trim();
}

export const hostKeyCreated = () => created;
