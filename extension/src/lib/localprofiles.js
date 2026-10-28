// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Locally imported profiles (settings → 导入本地对策文件), kept in
// storage.local as [{ profile, importedAt, replaces? }]. A local file may
// not quietly take over a packaged profile's id (security review L7): the
// settings page asks first, and only an entry that records the confirmation
// (`replaces`: the packaged version it replaces) is used; it is marked
// "未审核" like every local profile. Pure.

/** The entry as stored when importing a profile; replaces: the packaged version confirmed, or ''. */
export function localEntry(profile, { now = new Date(), replaces = '' } = {}) {
  const e = { profile, importedAt: new Date(now).toISOString().slice(0, 10) };
  if (replaces) e.replaces = String(replaces);
  return e;
}

/** Whether an entry with a packaged profile's id was confirmed to replace it. */
export function confirmedReplacement(entry) {
  return !!entry && typeof entry.replaces === 'string' && entry.replaces.length > 0;
}

/**
 * The stored entries that are in force: well-formed, and — for a packaged
 * profile's id — confirmed on import. packagedIds: the packaged profiles' ids.
 */
export function activeLocalEntries(entries, packagedIds = []) {
  const packaged = new Set(packagedIds);
  return (Array.isArray(entries) ? entries : []).filter((e) => e && e.profile && typeof e.profile === 'object' && (!packaged.has(e.profile.id) || confirmedReplacement(e)));
}

/** The stored list with this entry in place of any earlier one of the same id. */
export function withLocalEntry(entries, entry) {
  return [...(Array.isArray(entries) ? entries : []).filter((x) => !(x && x.profile && x.profile.id === entry.profile.id)), entry];
}
