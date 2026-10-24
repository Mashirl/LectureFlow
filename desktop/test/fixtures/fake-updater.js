// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Stand-in for electron-updater's autoUpdater in end-to-end tests of the
// unpackaged app (LECTUREFLOW_FAKE_UPDATER). Always finds version 9.9.9;
// "installing" writes LECTUREFLOW_FAKE_UPDATER_MARK instead of running an
// installer.
'use strict';

const fs = require('node:fs');
const { EventEmitter } = require('node:events');

class FakeUpdater extends EventEmitter {
  constructor() {
    super();
    this.checks = [];
  }

  async checkForUpdates() {
    this.checks.push({ allowPrerelease: this.allowPrerelease });
    setTimeout(() => this.emit('update-available', { version: '9.9.9' }), 10);
    return { updateInfo: { version: '9.9.9' } };
  }

  async downloadUpdate() {
    this.emit('download-progress', { percent: 30 });
    await new Promise((r) => setTimeout(r, 50));
    this.emit('update-downloaded', { version: '9.9.9' });
    return [];
  }

  quitAndInstall(isSilent, isForceRunAfter) {
    const mark = process.env.LECTUREFLOW_FAKE_UPDATER_MARK;
    if (mark) fs.writeFileSync(mark, JSON.stringify({ isSilent, isForceRunAfter, checks: this.checks, at: Date.now() }));
  }
}

module.exports = new FakeUpdater();
