// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Keeps the system awake while transcription or API tasks run (SPEC-v3 V1.1).
// 'prevent-app-suspension' lets the screen turn off. The blocker is injected
// (Electron's powerSaveBlocker in the app, a fake in tests).
'use strict';

const BLOCK_TYPE = 'prevent-app-suspension';

class SleepGuard {
  constructor({ blocker, log }) {
    this.blocker = blocker;
    this.log = log || { info() {} };
    this.id = null;
  }

  get active() {
    return this.id !== null;
  }

  /** update(want): start or stop the blocker so that it matches want. */
  update(want) {
    if (want && this.id === null) {
      this.id = this.blocker.start(BLOCK_TYPE);
      this.log.info('preventing system sleep while tasks run');
    } else if (!want && this.id !== null) {
      this.release();
    }
  }

  release() {
    if (this.id === null) return;
    try {
      if (this.blocker.isStarted(this.id)) this.blocker.stop(this.id);
    } finally {
      this.id = null;
      this.log.info('system sleep allowed again');
    }
  }
}

module.exports = { SleepGuard, BLOCK_TYPE };
