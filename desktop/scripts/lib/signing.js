// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Which signing material the build environment provides (SPEC-v3 V9). Only
// environment variables are read; none of them present = unsigned build.
'use strict';

function signingFrom(env) {
  const has = (...names) => names.every((n) => typeof env[n] === 'string' && env[n].trim() !== '');
  const macCert = has('CSC_LINK') || has('CSC_NAME');
  const macNotarize = macCert && (has('APPLE_API_KEY', 'APPLE_API_KEY_ID', 'APPLE_API_ISSUER') || has('APPLE_ID', 'APPLE_APP_SPECIFIC_PASSWORD', 'APPLE_TEAM_ID'));
  const winCert = has('WIN_CSC_LINK') || has('CSC_LINK');
  return { macCert, macNotarize, winCert };
}

// The signing variables electron-builder reads. A workflow passes a secret
// that is not set as an empty string, and electron-builder takes an empty
// CSC_LINK for a path (the project directory) and fails on it.
const SIGNING_VARS = ['CSC_LINK', 'CSC_KEY_PASSWORD', 'CSC_NAME', 'WIN_CSC_LINK', 'WIN_CSC_KEY_PASSWORD',
  'APPLE_API_KEY', 'APPLE_API_KEY_ID', 'APPLE_API_ISSUER', 'APPLE_ID', 'APPLE_APP_SPECIFIC_PASSWORD', 'APPLE_TEAM_ID'];

// dropEmptySigning deletes the signing variables of env that are blank, so
// that they count as absent for electron-builder as for signingFrom.
function dropEmptySigning(env) {
  for (const n of SIGNING_VARS) if (typeof env[n] === 'string' && env[n].trim() === '') delete env[n];
  return env;
}

module.exports = { signingFrom, dropEmptySigning };
