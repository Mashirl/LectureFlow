// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// "Use the default location" on the error page for an unreadable
// location.json: the file is renamed (never deleted) so that the default
// data directory is used; nothing in any data directory is touched, and
// renaming the file back restores the old setting.
'use strict';

const fs = require('node:fs');

function stamp(d) {
  const p = (n) => String(n).padStart(2, '0');
  return `${d.getFullYear()}${p(d.getMonth() + 1)}${p(d.getDate())}-${p(d.getHours())}${p(d.getMinutes())}${p(d.getSeconds())}`;
}

/** backupName(locationFile, exists, now) → location.json.bak, or a dated name if that exists. */
function backupName(locationFile, exists = fs.existsSync, now = new Date()) {
  const plain = `${locationFile}.bak`;
  if (!exists(plain)) return plain;
  const base = `${locationFile}.bak-${stamp(now)}`;
  for (let i = 0; ; i++) {
    const name = i === 0 ? base : `${base}-${i}`;
    if (!exists(name)) return name;
  }
}

/** setLocationAside(locationFile) → the backup path (throws when the rename fails). */
function setLocationAside(locationFile, { exists = fs.existsSync, rename = fs.renameSync, now = new Date() } = {}) {
  const target = backupName(locationFile, exists, now);
  rename(locationFile, target);
  return target;
}

module.exports = { backupName, setLocationAside };
