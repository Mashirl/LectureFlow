// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The update repository of a build (SPEC-v3 V8, gate fix 1). It comes only
// from the build environment:
//
//   LECTUREFLOW_UPDATE_REPO     "owner/name" of the GitHub repository whose
//                               releases the app checks. Unset or empty: no
//                               publish configuration, no app-update.yml,
//                               no latest*.yml; the packaged app never loads
//                               the updater. Any other value must be a valid
//                               owner/name, else the build fails.
//   LECTUREFLOW_PUBLISHER_NAME  Windows only, with a signing certificate: the
//                               publisher name (as in the certificate) that
//                               downloaded updates must be signed by. Without
//                               it electron-builder takes the certificate's
//                               common name. Ignored for unsigned builds.
'use strict';

const { parseRepoSlug, parseAppUpdateYml } = require('../../src/main/lib/updatefeed');

const REPO_VAR = 'LECTUREFLOW_UPDATE_REPO';
const PUBLISHER_VAR = 'LECTUREFLOW_PUBLISHER_NAME';

/** updateRepoFromEnv(env) → { owner, repo } or null; throws on a malformed value. */
function updateRepoFromEnv(env) {
  const raw = env[REPO_VAR];
  if (raw === undefined || raw === '') return null;
  const slug = parseRepoSlug(raw);
  if (!slug) {
    const shown = JSON.stringify(String(raw).slice(0, 120));
    throw new Error(`${REPO_VAR} must be a GitHub repository as "owner/name" (letters, digits, "-", "_", "."), got ${shown}`);
  }
  return slug;
}

/** publisherNameFromEnv(env, winCert) → the explicit publisher name or null. */
function publisherNameFromEnv(env, winCert) {
  const v = typeof env[PUBLISHER_VAR] === 'string' ? env[PUBLISHER_VAR].trim() : '';
  if (!winCert || !v) return null;
  if (v.length > 256 || /[\u0000-\u001f\u007f]/.test(v)) throw new Error(`${PUBLISHER_VAR} must be a single line of at most 256 characters`);
  return v;
}

/**
 * updateBuildConfig(env, { winCert }) → {
 *   repo: { owner, repo } | null,
 *   publish: electron-builder publish (null = no feed, no app-update.yml),
 *   winPublisherName: string | null,
 *   summary: one line for the build log and the size report }
 */
function updateBuildConfig(env, { winCert = false } = {}) {
  const repo = updateRepoFromEnv(env);
  if (!repo) {
    return {
      repo: null,
      publish: null, // explicit null: electron-builder must not guess a repository (package.json, git remote, GH_TOKEN)
      winPublisherName: null,
      summary: `Automatic updates: off (${REPO_VAR} is not set: no update repository configured, no app-update.yml)`,
    };
  }
  const winPublisherName = publisherNameFromEnv(env, winCert);
  return {
    repo,
    publish: [{ provider: 'github', owner: repo.owner, repo: repo.repo, releaseType: 'draft' }],
    winPublisherName,
    summary: `Automatic updates: on (GitHub releases of ${repo.owner}/${repo.repo})`,
  };
}

/**
 * checkUpdateFeed({ repo, appUpdateYml, distFiles, platform, winCert }) →
 * problems found in a finished build (empty = consistent):
 *   - no repository: no app-update.yml in the resources, no latest*.yml
 *   - repository: app-update.yml names exactly that GitHub repository
 *   - repository + signed Windows build: app-update.yml has a publisherName
 *     (electron-updater only verifies update signatures when it has one)
 * appUpdateYml is the file's text or null when it does not exist.
 */
function checkUpdateFeed({ repo, appUpdateYml, distFiles = [], platform, winCert = false }) {
  const problems = [];
  const latest = distFiles.filter((f) => /^latest.*\.ya?ml$/i.test(f));
  if (!repo) {
    if (appUpdateYml !== null) problems.push('app-update.yml was generated although no update repository is configured');
    if (latest.length) problems.push(`update metadata was generated although no update repository is configured: ${latest.join(', ')}`);
    return problems;
  }
  if (appUpdateYml === null) {
    problems.push('app-update.yml is missing although an update repository is configured');
    return problems;
  }
  const y = parseAppUpdateYml(appUpdateYml);
  if (y.provider !== 'github' || y.owner !== repo.owner || y.repo !== repo.repo) {
    problems.push(`app-update.yml points at ${y.provider || '?'}:${y.owner || '?'}/${y.repo || '?'}, expected github:${repo.owner}/${repo.repo}`);
  }
  if (platform === 'win32' && winCert) {
    const names = Array.isArray(y.publisherName) ? y.publisherName : (y.publisherName ? [y.publisherName] : []);
    if (!names.length) problems.push('signed Windows build without publisherName in app-update.yml: update signatures would not be verified');
  }
  return problems;
}

module.exports = { REPO_VAR, PUBLISHER_VAR, updateRepoFromEnv, publisherNameFromEnv, updateBuildConfig, checkUpdateFeed };
