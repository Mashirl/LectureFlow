// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Application updates (SPEC-v3 V8) with electron-updater and GitHub Releases.
//
// The page never talks to the updater: the main process reports the
// updater's state to the service (POST /api/app/update/state); the page
// shows it and asks for a download or for installing on quit; the main
// process reads those requests back in its 5-second poll (sync()). So the
// preload whitelist stays as it is.
//
//   - checks at start and every 6 hours; the channel comes from
//     /api/app/settings (update_channel "beta" = pre-releases allowed)
//   - downloads only after the user asked (autoDownload off)
//   - installs only in the quit flow, after the service and every task in
//     it have stopped, and only if the user asked to install on quit
//
// No Electron imports: the updater and the API client are injected.
'use strict';

const CHECK_INTERVAL_MS = 6 * 60 * 60 * 1000;
const PROGRESS_REPORT_MS = 1000;

/** prereleaseFor: settings.update_channel "beta" allows pre-releases. */
function prereleaseFor(settings) {
  return !!settings && settings.update_channel === 'beta';
}

/** shouldDownload: the page asked and there is something to download. */
function shouldDownload(local, remote) {
  if (!remote || !remote.download_requested || local.busy) return false;
  return local.state === 'available' || (local.state === 'error' && !!local.version && local.failed === 'download');
}

/**
 * installOnQuit: install the downloaded update now that the app quits?
 * Only when it is downloaded, the user asked for it, and the service has
 * stopped (no task runs any more: updates are never installed under a task).
 */
function installOnQuit({ state, requested, serviceStopped }) {
  return state === 'ready' && requested === true && serviceStopped === true;
}

class UpdateController {
  /**
   * opts: updater (electron-updater's autoUpdater or a fake), api ({ settings,
   * getUpdate, postUpdateState }), log, timers ({ setInterval, clearInterval }),
   * now()
   */
  constructor({ updater, api, log, timers = { setInterval, clearInterval }, now = Date.now }) {
    this.updater = updater;
    this.api = api;
    this.log = log || { info() {}, warn() {} };
    this.timers = timers;
    this.now = now;
    this.state = 'none';
    this.version = '';
    this.percent = 0;
    this.errorCode = null;
    this.failed = null; // 'check' | 'download'
    this.busy = false; // a check or download is running
    this.requested = false; // install on quit (from the page)
    this.timer = null;
    this.started = false;
    this.lastProgress = 0;
    this.reporting = Promise.resolve();
  }

  start() {
    if (this.started) return;
    this.started = true;
    const u = this.updater;
    u.autoDownload = false;
    u.autoInstallOnAppQuit = false;
    u.allowDowngrade = false;
    u.on('update-available', (info) => this._set('available', { version: info && info.version }));
    u.on('update-not-available', () => {
      if (this.state !== 'ready') this._set('none');
    });
    u.on('download-progress', (p) => {
      const t = this.now();
      if (t - this.lastProgress < PROGRESS_REPORT_MS) return;
      this.lastProgress = t;
      this._set('downloading', { version: this.version, percent: Math.max(0, Math.min(100, Math.round((p && p.percent) || 0))) });
    });
    u.on('update-downloaded', (info) => this._set('ready', { version: (info && info.version) || this.version }));
    u.on('error', (err) => this._fail(this.busy === 'download' ? 'download' : 'check', err));
    this.check();
    this.timer = this.timers.setInterval(() => this.check(), CHECK_INTERVAL_MS);
  }

  stop() {
    if (this.timer) this.timers.clearInterval(this.timer);
    this.timer = null;
  }

  /** check: one update check (skipped while downloading or once downloaded). */
  async check() {
    if (this.busy || this.state === 'downloading' || this.state === 'ready') return;
    this.busy = 'check';
    try {
      const settings = await this.api.settings().catch(() => null);
      this.updater.allowPrerelease = prereleaseFor(settings);
      await this.updater.checkForUpdates();
    } catch (err) {
      this._fail('check', err);
    } finally {
      this.busy = false;
    }
  }

  /** sync: read the page's requests (called from the 5-second poll). */
  async sync() {
    let remote;
    try {
      remote = await this.api.getUpdate();
    } catch {
      return;
    }
    if (!remote) return;
    this.requested = remote.install_on_quit === true;
    // After a service restart its memory is empty: report again.
    if (remote.state !== this.state || (remote.version || '') !== this.version) this._report();
    if (shouldDownload(this, remote)) await this.download();
  }

  async download() {
    this.busy = 'download';
    this.failed = null;
    this._set('downloading', { version: this.version, percent: 0 });
    try {
      await this.updater.downloadUpdate();
    } catch (err) {
      this._fail('download', err);
    } finally {
      this.busy = false;
    }
  }

  /**
   * quitAndInstall: called by the quit flow once the service has stopped.
   * Returns true when the installer was started (it quits the app itself).
   */
  quitAndInstall({ serviceStopped }) {
    if (!installOnQuit({ state: this.state, requested: this.requested, serviceStopped })) return false;
    this.log.info(`installing update ${this.version} on quit`);
    try {
      this.updater.quitAndInstall(true, false); // silent, do not start the app again
      return true;
    } catch (err) {
      this.log.warn(`install update: ${err && err.message}`);
      return false;
    }
  }

  _fail(step, err) {
    this.log.warn(`update ${step} failed: ${err && err.message ? err.message : err}`);
    this.failed = step;
    this._set('error', { version: this.version, code: step === 'download' ? 'update.download_failed' : 'update.check_failed' });
  }

  _set(state, { version, percent, code } = {}) {
    this.state = state;
    if (version !== undefined) this.version = version || '';
    if (state === 'none') this.version = '';
    this.percent = state === 'ready' ? 100 : percent || 0;
    this.errorCode = state === 'error' ? code : null;
    this._report();
  }

  _report() {
    const body = { state: this.state };
    if (this.version) body.version = this.version;
    if (this.state === 'downloading') body.percent = this.percent;
    if (this.state === 'error') body.error = { code: this.errorCode };
    // Reports go out one after another, in order.
    this.reporting = this.reporting
      .then(() => this.api.postUpdateState(body))
      .catch((err) => this.log.warn(`report update state: ${err && err.message}`));
    return this.reporting;
  }
}

module.exports = { UpdateController, prereleaseFor, shouldDownload, installOnQuit, CHECK_INTERVAL_MS };
