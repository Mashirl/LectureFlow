// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Window-close and sleep decisions (SPEC-v3 V1.1). Pure functions over the
// service's /api/app/activity and /api/app/settings answers.
'use strict';

const CLOSE_BEHAVIORS = ['ask', 'background', 'quit'];

function runningCount(activity) {
  const n = activity && activity.running;
  return Number.isFinite(n) && n > 0 ? Math.floor(n) : 0;
}

/**
 * decideClose: what closing the main window does.
 *   'quit'       stop the service and exit
 *   'background' keep running without the window (tray on Windows, dock on macOS)
 *   'ask'        show the close dialog
 *   'hide'       macOS without running tasks: close the window, stay in the dock
 * activity is null when the service could not be asked (it then cannot be
 * doing anything worth keeping alive for).
 */
function decideClose({ platform, activity, settings }) {
  if (runningCount(activity) === 0) return platform === 'darwin' ? 'hide' : 'quit';
  const b = settings && settings.close_behavior;
  if (b === 'background' || b === 'quit') return b;
  return 'ask';
}

// Buttons of the close dialog, in order. Esc / closing the dialog = cancel.
const CLOSE_DIALOG_BUTTONS = ['background', 'quit', 'cancel'];

/**
 * interpretCloseDialog turns showMessageBox's { response, checkboxChecked }
 * into { action: 'background'|'quit'|'cancel', remember: 'background'|'quit'|null }.
 */
function interpretCloseDialog(result) {
  const action = CLOSE_DIALOG_BUTTONS[result && result.response] || 'cancel';
  const remember = action !== 'cancel' && !!(result && result.checkboxChecked) ? action : null;
  return { action, remember };
}

/**
 * wantSleepBlock: keep the system awake while tasks run and the setting
 * allows it. Unknown settings fall back to the default (prevent_sleep: true).
 */
function wantSleepBlock(activity, settings) {
  // A device self-test only sets transcribing (it is not a running job).
  if (runningCount(activity) === 0 && !(activity && activity.transcribing)) return false;
  return !(settings && settings.prevent_sleep === false);
}

module.exports = { CLOSE_BEHAVIORS, CLOSE_DIALOG_BUTTONS, runningCount, decideClose, interpretCloseDialog, wantSleepBlock };
