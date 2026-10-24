// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Debugging switches of the packaged app (SPEC-v3 V2, acceptance V12.2
// finding 11). The Electron fuses turn off Node's --inspect, but Chromium's
// own --remote-debugging-port / --remote-debugging-pipe stay available:
// whoever starts LectureFlow with them could drive the window over the
// DevTools protocol and read the requests that carry the access token. The
// packaged app therefore refuses to start with any of them.
//
// LECTUREFLOW_E2E_ALLOW_DEBUG=1 lets them through (acceptance runs drive the
// packaged app over CDP). Like the command line itself it can only be set by
// a local user who starts the program, so it is a test entry, not a security
// boundary.
//
// No Electron imports.
'use strict';

// Chromium and Node switches that open a debugging channel into the app.
const DEBUG_SWITCHES = [
  'remote-debugging-port',
  'remote-debugging-pipe',
  'remote-debugging-address',
  'remote-debugging-io-pipes',
  'remote-allow-origins',
  'inspect',
  'inspect-brk',
  'inspect-port',
  'inspect-publish-uid',
  'debug',
  'debug-brk',
];

/**
 * debugSwitchesIn(argv) → the debugging switches named on a command line.
 * Chromium also accepts "-name" and, on Windows, "/name"; switch names are
 * compared without case; nothing after a bare "--" is a switch.
 */
function debugSwitchesIn(argv, platform = process.platform) {
  const found = [];
  const prefix = platform === 'win32' ? /^(?:--|-|\/)/ : /^(?:--|-)/;
  for (const raw of argv || []) {
    const a = String(raw);
    if (a === '--') break;
    if (!prefix.test(a)) continue;
    const name = a.replace(prefix, '').split('=')[0].toLowerCase();
    if (DEBUG_SWITCHES.includes(name) && !found.includes(name)) found.push(name);
  }
  return found;
}

/**
 * refuseDebugging({ isPackaged, argv, env, hasSwitch, platform }) → the
 * debugging switches that make the packaged app refuse to start (empty =
 * start). hasSwitch is app.commandLine.hasSwitch (Chromium's own parse).
 */
function refuseDebugging({ isPackaged, argv, env = {}, hasSwitch = () => false, platform = process.platform }) {
  if (!isPackaged) return [];
  if (env.LECTUREFLOW_E2E_ALLOW_DEBUG === '1') return [];
  const found = debugSwitchesIn(argv, platform);
  for (const s of DEBUG_SWITCHES) {
    let on = false;
    try {
      on = !!hasSwitch(s);
    } catch {
      on = false;
    }
    if (on && !found.includes(s)) found.push(s);
  }
  return found;
}

module.exports = { DEBUG_SWITCHES, debugSwitchesIn, refuseDebugging };
