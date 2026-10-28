// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// What the service says about a long or failed start, besides the ready
// line and "ERROR: <reason>" (cmd/lectureflow/startfail.go). Pure functions.
//
//   stdout  "LECTUREFLOW_STARTING <step>"   while a long step runs before the
//           ready line (the backup before an upgrade, the migrations, the
//           sealing of credentials, the import backups' clean-up): once a
//           second into the step, then every 5 s. ServerProcess keeps
//           waiting while they come (test-upgrade F3: a large database's
//           first start took longer than 30 seconds and was killed).
//   stderr  "LECTUREFLOW_FAILURE {"code":"startup.…","params":{…}}"  just
//           before the ERROR line of a failure the user can act on; the
//           error page shows startFailureText in the interface language and
//           the ERROR line (Chinese and English) under "Details".
'use strict';

const STARTING_PREFIX = 'LECTUREFLOW_STARTING ';
const FAILURE_PREFIX = 'LECTUREFLOW_FAILURE ';

/** parseStartingLine: the step a "LECTUREFLOW_STARTING <step>" line names, or null. */
function parseStartingLine(line) {
  if (typeof line !== 'string' || !line.startsWith(STARTING_PREFIX)) return null;
  const step = line.slice(STARTING_PREFIX.length).trim();
  return /^[a-z][a-z0-9-]{0,40}$/.test(step) ? step : null;
}

const str = (v) => (typeof v === 'string' && v.length <= 1000 ? v : null);
const num = (v) => (typeof v === 'number' && Number.isFinite(v) && v >= 0 ? v : null);

/**
 * extractFailure(lines): { code, params } of the last LECTUREFLOW_FAILURE
 * line of stderr, or null (none, or not valid JSON with a startup.* code).
 */
function extractFailure(lines) {
  for (let i = (lines || []).length - 1; i >= 0; i--) {
    const l = lines[i];
    if (typeof l !== 'string' || !l.startsWith(FAILURE_PREFIX)) continue;
    let v;
    try {
      v = JSON.parse(l.slice(FAILURE_PREFIX.length));
    } catch {
      return null;
    }
    if (!v || typeof v.code !== 'string' || !/^startup\.[a-z_]{1,60}$/.test(v.code)) return null;
    const params = v.params && typeof v.params === 'object' && !Array.isArray(v.params) ? v.params : {};
    return { code: v.code, params };
  }
  return null;
}

/** gb(bytes): "3.1" (at least "0.1"), the number the texts put before "GB". */
function gb(bytes) {
  const n = Math.max(0.1, Math.round((bytes / 2 ** 30) * 10) / 10);
  return n.toFixed(1);
}

/**
 * startFailureText(failure, t): the error page's text for a known code in
 * the interface language (desktop/locales "startfail.*"), or null (an
 * unknown code, or its parameters missing: the generic text then).
 */
function startFailureText(failure, t) {
  if (!failure) return null;
  const p = failure.params || {};
  switch (failure.code) {
    case 'startup.data_dir_in_use': {
      const dir = str(p.data_dir);
      if (dir === null) return null;
      const pid = num(p.pid);
      return pid ? t('startfail.data_dir_in_use', { pid: String(pid), path: dir }) : t('startfail.data_dir_in_use_nopid', { path: dir });
    }
    case 'startup.data_dir_newer': {
      const found = num(p.found);
      const known = num(p.known);
      const backups = str(p.backups);
      if (found === null || known === null || backups === null) return null;
      return t('startfail.data_dir_newer', { found: String(found), known: String(known), path: backups });
    }
    case 'startup.upgrade_backup_no_space': {
      const need = num(p.need_bytes);
      const free = num(p.free_bytes);
      const dir = str(p.dir);
      if (need === null || free === null || dir === null) return null;
      return t('startfail.upgrade_backup_no_space', { need: gb(need), free: gb(free), path: dir });
    }
    case 'startup.upgrade_backup_failed': {
      const dir = str(p.dir);
      if (dir === null) return null;
      return t('startfail.upgrade_backup_failed', { path: dir });
    }
    default:
      return null;
  }
}

/**
 * isEnvironmentFailure: the start failed because of something outside this
 * version (another LectureFlow holds the data folder): a freshly installed
 * version that meets it has not failed its probation (no rollback).
 */
function isEnvironmentFailure(failure) {
  return !!failure && failure.code === 'startup.data_dir_in_use';
}

module.exports = {
  STARTING_PREFIX,
  FAILURE_PREFIX,
  parseStartingLine,
  extractFailure,
  startFailureText,
  isEnvironmentFailure,
  gb,
};
