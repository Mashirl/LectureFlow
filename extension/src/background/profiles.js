// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Profiles: the reviewed ones packaged with the extension (profiles/index.json,
// written by the build) and the user's local imports (storage.local, written
// by the options page, marked "未审核"). Local ones come first so a
// contributor can try a new version of a packaged profile — but a local
// file with a packaged profile's id is used only when the user confirmed,
// on import, that it replaces the packaged one (the entry says which
// version: `replaces`; security review L7); otherwise the packaged profile
// stays in force. A local profile that no longer passes the schema is
// skipped, never trusted.

import { browser } from './env.js';
import { checkProfile } from '../lib/profile.js';
import { activeLocalEntries } from '../lib/localprofiles.js';

const LOCAL_KEY = 'localProfiles';
let bundledCache = null;
let schemaCache = null;

export async function profileSchema() {
  if (!schemaCache) schemaCache = await (await fetch(browser.runtime.getURL('schema/profile.schema.json'))).json();
  return schemaCache;
}

async function bundled() {
  if (bundledCache) return bundledCache;
  const out = [];
  try {
    const index = await (await fetch(browser.runtime.getURL('profiles/index.json'))).json();
    for (const file of index.profiles || []) {
      const p = await (await fetch(browser.runtime.getURL(`profiles/${file}`))).json();
      out.push({ profile: p, reviewed: true, source: 'bundled' });
    }
  } catch {
    // a build without profiles
  }
  bundledCache = out;
  return out;
}

export async function localProfiles() {
  const stored = (await browser.storage.local.get(LOCAL_KEY))[LOCAL_KEY];
  return Array.isArray(stored) ? stored : [];
}

/** All profiles in priority order: [{ profile, reviewed, source }]. */
export async function allProfiles() {
  const schema = await profileSchema();
  const packaged = await bundled();
  const local = activeLocalEntries(await localProfiles(), packaged.map((e) => e.profile.id))
    .filter((e) => checkProfile(schema, e.profile).length === 0)
    .map((e) => ({ profile: e.profile, reviewed: false, source: 'local' }));
  return [...local, ...packaged];
}
