// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Where application updates come from (SPEC-v3 V8). The update repository is
// fixed at build time (LECTUREFLOW_UPDATE_REPO, scripts/lib/updaterepo.js);
// without it the build has no publish configuration and no app-update.yml,
// and the packaged app keeps the updater switched off. This module is shared
// by the build (format check of the repository name) and the main process
// (reading resources/app-update.yml before electron-updater is loaded).
//
// No Electron imports.
'use strict';

const path = require('node:path');

// GitHub owner: 1–39 letters, digits or single inner hyphens.
const OWNER_RE = /^[A-Za-z0-9](?:[A-Za-z0-9]|-(?=[A-Za-z0-9])){0,38}$/;
// GitHub repository name: 1–100 of letters, digits, ".", "_", "-".
const REPO_RE = /^[A-Za-z0-9._-]{1,100}$/;

/**
 * parseRepoSlug("owner/name") → { owner, repo } or null. Strict: exactly one
 * slash, no surrounding spaces, no URL, no ".git" suffix, not "." or "..".
 */
function parseRepoSlug(s) {
  if (typeof s !== 'string') return null;
  const parts = s.split('/');
  if (parts.length !== 2) return null;
  const [owner, repo] = parts;
  if (!OWNER_RE.test(owner) || !REPO_RE.test(repo)) return null;
  if (repo === '.' || repo === '..' || /\.git$/i.test(repo)) return null;
  return { owner, repo };
}

/**
 * parseAppUpdateYml(text) → the top-level "key: value" pairs of the file
 * electron-builder writes (provider, owner, repo, ...). Quoted values are
 * unquoted; nested lists (publisherName) are collected as arrays.
 */
function parseAppUpdateYml(text) {
  const out = {};
  let listKey = null;
  for (const raw of String(text).split(/\r?\n/)) {
    if (!raw.trim() || raw.trim().startsWith('#')) continue;
    const item = /^\s+-\s*(.*)$/.exec(raw);
    if (item && listKey) {
      out[listKey].push(unquote(item[1]));
      continue;
    }
    const kv = /^([A-Za-z][A-Za-z0-9_]*):\s*(.*)$/.exec(raw);
    if (!kv) {
      listKey = null;
      continue;
    }
    if (kv[2] === '') {
      listKey = kv[1];
      out[listKey] = [];
    } else {
      listKey = null;
      out[kv[1]] = unquote(kv[2]);
    }
  }
  return out;
}

function unquote(v) {
  const s = v.trim();
  if (s.length >= 2 && ((s[0] === '"' && s.endsWith('"')) || (s[0] === "'" && s.endsWith("'")))) return s.slice(1, -1);
  return s;
}

/**
 * updateFeedFrom({ resourcesPath, readFile }) → the update feed of a
 * packaged app:
 *   { enabled: false, reason: 'not_configured' }  no app-update.yml (built
 *                                                  without LECTUREFLOW_UPDATE_REPO)
 *   { enabled: false, reason: 'invalid', detail }  unreadable or not a GitHub
 *                                                  feed with a valid owner/name
 *   { enabled: true, owner, repo }
 */
function updateFeedFrom({ resourcesPath, readFile }) {
  const file = path.join(resourcesPath, 'app-update.yml');
  let text;
  try {
    text = readFile(file);
  } catch (err) {
    if (err && err.code === 'ENOENT') return { enabled: false, reason: 'not_configured' };
    return { enabled: false, reason: 'invalid', detail: `cannot read ${file}: ${err && err.code ? err.code : err}` };
  }
  const y = parseAppUpdateYml(text);
  if (y.provider !== 'github') return { enabled: false, reason: 'invalid', detail: `provider ${JSON.stringify(y.provider || '')} is not github` };
  const slug = parseRepoSlug(`${y.owner || ''}/${y.repo || ''}`);
  if (!slug) return { enabled: false, reason: 'invalid', detail: 'owner/repo is not a valid GitHub repository name' };
  return { enabled: true, owner: slug.owner, repo: slug.repo };
}

/** updateFeedNote(feed) → the log line for a feed (English, no secrets). */
function updateFeedNote(feed) {
  if (feed.enabled) return `updates: checking GitHub releases of ${feed.owner}/${feed.repo}`;
  if (feed.reason === 'not_configured') return 'updates: no update repository was configured for this build; automatic updates are off';
  return `updates: the update configuration of this build is not usable (${feed.detail}); automatic updates are off`;
}

module.exports = { parseRepoSlug, parseAppUpdateYml, updateFeedFrom, updateFeedNote };
