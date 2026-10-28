// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The desktop shell's part of the browser extension's Native Messaging
// (SPEC-ext, .claude/ext-brief.md §4). Pure functions (isReadOnlyVolume
// asks the file system).
//
//   - The service registers the native host and writes launch.json with the
//     program that starts this app (-launcher); the host runs it with
//     --background when the extension connects while LectureFlow is not
//     running (on macOS through open -n -g -a LectureFlow.app). A macOS app
//     that runs from its disk image or translocated passes
//     -launcher-blocked instead (mac-fix): launch.json would name a path
//     that disappears.
//   - --background: start in the tray (Windows) or Dock (macOS) without a
//     window; a second start with it does not show the window either.
//   - "LECTUREFLOW_OPEN <route>" on the service's stdout: the extension's
//     "在 LectureFlow 中打开"; show the window on that route.
'use strict';

const fs = require('node:fs');

const BACKGROUND_ARG = '--background';
const OPEN_PREFIX = 'LECTUREFLOW_OPEN ';
// The web app's hash routes the service sends (#/lecture/ID, #/course/ID).
const ROUTE_RE = /^#\/[A-Za-z0-9._~%/-]{0,200}$/;

/** isBackgroundLaunch: argv (process.argv or second-instance's) asks for no window. */
function isBackgroundLaunch(argv, additionalData) {
  if (additionalData && additionalData.background === true) return true;
  return Array.isArray(argv) && argv.some((a) => a === BACKGROUND_ARG);
}

/** isReadOnlyVolume: dir is on a read-only file system (a mounted disk image). */
function isReadOnlyVolume(dir) {
  try {
    fs.accessSync(dir, fs.constants.W_OK);
    return false;
  } catch (err) {
    return !!err && err.code === 'EROFS';
  }
}

/**
 * launcherBlocked: why the native host could not start this app from where
 * it runs, or null (packaged macOS only; the service then gets
 * -launcher-blocked instead of -launcher and the diagnostics ask to move
 * the app to Applications):
 *   'translocated'  Gatekeeper's App Translocation (a random read-only
 *                   path under …/AppTranslocation/ that is gone after quit)
 *   'dmg'           a read-only volume under /Volumes: the disk image it was
 *                   opened from, gone once ejected (a writable external disk
 *                   is the user's choice and allowed)
 */
function launcherBlocked({ isPackaged, platform, execPath, readOnly = isReadOnlyVolume }) {
  if (!isPackaged || platform !== 'darwin' || !execPath) return null;
  if (execPath.includes('/AppTranslocation/')) return 'translocated';
  const vol = /^\/Volumes\/[^/]+/.exec(execPath);
  if (vol && readOnly(vol[0])) return 'dmg';
  return null;
}

/**
 * launcherFor: the program the native host starts the app with, or null.
 *   packaged Windows  the portable exe (PORTABLE_EXECUTABLE_FILE) or the installed exe
 *   packaged macOS    the executable inside LectureFlow.app (process.execPath),
 *                     unless it runs from a place that disappears (launcherBlocked)
 *   not packaged      null: the development Electron cannot be started that way
 */
function launcherFor({ isPackaged, platform, execPath, env = {}, readOnly = isReadOnlyVolume }) {
  if (!isPackaged) return null;
  if (platform === 'win32' && env.PORTABLE_EXECUTABLE_FILE) return env.PORTABLE_EXECUTABLE_FILE;
  if (launcherBlocked({ isPackaged, platform, execPath, readOnly })) return null;
  return execPath || null;
}

/** isPortable: the packaged Windows portable build (electron-builder sets the variable). */
function isPortable({ isPackaged, platform, env = {} }) {
  return !!(isPackaged && platform === 'win32' && env.PORTABLE_EXECUTABLE_FILE);
}

/** parseOpenLine: "LECTUREFLOW_OPEN #/lecture/ID" → "#/lecture/ID", anything else → null. */
function parseOpenLine(line) {
  if (typeof line !== 'string' || !line.startsWith(OPEN_PREFIX)) return null;
  const route = line.slice(OPEN_PREFIX.length).trim();
  if (!ROUTE_RE.test(route) || route.includes('..')) return null;
  return route;
}

module.exports = { BACKGROUND_ARG, OPEN_PREFIX, isBackgroundLaunch, launcherFor, launcherBlocked, isReadOnlyVolume, isPortable, parseOpenLine };
