// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Entry point of the LectureFlow desktop shell (SPEC-v3 V1, V2).
'use strict';

const fs = require('node:fs');
const path = require('node:path');
const { app, dialog, Menu } = require('electron');

const { resolveDataDir } = require('./lib/datadir');
const { refuseDebugging } = require('./lib/debugflags');
const { LOCALE_FILE, createShellTranslator, loadTranslator, readLocaleMemo } = require('./lib/i18n');
const { buildMenuTemplate } = require('./lib/menu');
const { updateFeedFrom, updateFeedNote } = require('./lib/updatefeed');
const { createLogger } = require('./logger');
const { Shell, appDataRoot } = require('./shell');

// Windows notifications are grouped under this id; keep it equal to the
// appId of the installer configuration (step 8).
const APP_ID = 'org.lectureflow.desktop';

/**
 * loadUpdater: electron-updater in the packaged app, only when the build
 * configured an update repository (LECTUREFLOW_UPDATE_REPO at build time ->
 * resources/app-update.yml, see scripts/lib/updaterepo.js). Without it the
 * updater is never loaded: no update checks, /api/app/update stays "none".
 * Unpackaged builds have no update feed; tests may point
 * LECTUREFLOW_FAKE_UPDATER at a module that stands in for it.
 * Returns { updater, note }: note is logged once the log file is open.
 */
function loadUpdater(log) {
  if (!app.isPackaged) {
    const fake = process.env.LECTUREFLOW_FAKE_UPDATER;
    return { updater: fake ? require(path.resolve(fake)) : null, note: null };
  }
  const feed = updateFeedFrom({ resourcesPath: process.resourcesPath, readFile: (p) => fs.readFileSync(p, 'utf8') });
  const note = updateFeedNote(feed);
  if (!feed.enabled) return { updater: null, note };
  try {
    const { autoUpdater } = require('electron-updater');
    autoUpdater.logger = { info: (m) => log.info(`updater: ${m}`), warn: (m) => log.warn(`updater: ${m}`), error: (m) => log.warn(`updater: ${m}`), debug() {} };
    return { updater: autoUpdater, note };
  } catch (err) {
    return { updater: null, note: `updates unavailable: ${err.message}` };
  }
}

/**
 * refuseDebugSwitches: the packaged app does not start with a debugging
 * switch (--remote-debugging-port and the like, see lib/debugflags.js)
 * unless LECTUREFLOW_E2E_ALLOW_DEBUG=1. Returns true when it is quitting.
 * Runs first, before anything else of the app exists.
 */
function refuseDebugSwitches() {
  const refused = refuseDebugging({
    isPackaged: app.isPackaged,
    argv: process.argv.slice(1),
    env: process.env,
    hasSwitch: (s) => app.commandLine.hasSwitch(s),
  });
  if (!refused.length) return false;
  // app.getLocale() is not available before "ready" on Windows; ICU's default is.
  const { t } = loadTranslator(Intl.DateTimeFormat().resolvedOptions().locale);
  try {
    dialog.showErrorBox(t('debug.refused.title'), t('debug.refused.body', { switches: refused.map((s) => `--${s}`).join(', ') }));
  } catch {
    // no dialog: still refuse
  }
  app.exit(2);
  return true;
}

function main() {
  if (refuseDebugSwitches()) return;
  app.enableSandbox();
  const isDev = !app.isPackaged;
  const log = createLogger({ echo: isDev });

  // Chromium's profile goes to {default data root}/electron, or inside the
  // data dir given by LECTUREFLOW_DATA_DIR, so that tests and acceptance
  // runs with their own data dir also get their own single-instance lock.
  const resolved = resolveDataDir({ env: process.env, appData: appDataRoot(), readFile: (p) => fs.readFileSync(p, 'utf8') });
  app.setPath('userData', resolved.profileDir);

  if (!app.requestSingleInstanceLock()) {
    app.quit();
    return;
  }
  if (process.platform === 'win32') app.setAppUserModelId(APP_ID);

  let shellCtl = null;
  let stopped = false;

  app.on('second-instance', () => {
    if (shellCtl) shellCtl.showMainWindow();
  });
  app.on('web-contents-created', (event, contents) => {
    if (shellCtl) shellCtl.hardenContents(contents);
    else contents.on('will-navigate', (e) => e.preventDefault());
  });
  app.on('activate', () => {
    if (shellCtl) shellCtl.showMainWindow();
  });
  app.on('window-all-closed', () => {
    if (process.platform !== 'darwin') app.quit();
  });
  app.on('before-quit', (event) => {
    if (stopped || !shellCtl) return;
    event.preventDefault();
    shellCtl.shutdown().finally(() => {
      stopped = true;
      app.quit(); // when an update installer was started, it waits for this exit
    });
  });
  for (const sig of ['SIGINT', 'SIGTERM']) process.on(sig, () => app.quit());
  process.on('unhandledRejection', (err) => log.error(`unhandled rejection: ${err && err.stack ? err.stack : err}`));

  app.whenReady().then(() => {
    // The app's last language choice, so the splash already speaks it.
    const localeFile = path.join(app.getPath('userData'), LOCALE_FILE);
    const translator = createShellTranslator({ systemLocale: app.getLocale(), uiLocale: readLocaleMemo(localeFile) });
    const setMenu = () => {
      const menu = buildMenuTemplate({ platform: process.platform, isDev, t: translator.t, onOpenSettings: () => shellCtl && shellCtl.openSettings() });
      Menu.setApplicationMenu(menu ? Menu.buildFromTemplate(menu) : null);
    };
    setMenu();
    const { updater, note } = loadUpdater(log);
    shellCtl = new Shell({ translator, localeFile, onLocaleChange: setMenu, log, isDev, updater, updateNote: note });
    shellCtl.init();
  });
}

main();
