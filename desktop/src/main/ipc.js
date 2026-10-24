// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// IPC handlers behind the preload whitelist (v3-brief §3). Every handler
// first checks the sender (top frame of one of our windows, showing the
// local service) and then each argument (lib/validate.js).
'use strict';

const { app, BrowserWindow, dialog, ipcMain, Notification, powerMonitor, shell } = require('electron');

const { isServerUrl, isShellFileUrl } = require('./lib/security');
const {
  validateNoArgs, validatePickFiles, validateNotify, validateOpenExternal,
} = require('./lib/validate');

// Offline component packages (components.importFile). The archive format is
// the components manager's (internal/components); "All files" is offered too.
const COMPONENT_EXTENSIONS = ['zip'];

class SenderError extends Error {
  constructor() {
    super('IPC message from an untrusted sender');
    this.name = 'SenderError';
  }
}

/**
 * registerIpc(ctx) with ctx:
 *   getPort()            port of the running service (null otherwise)
 *   shellRoot            directory of the shell's own pages
 *   t                    translator
 *   showMainWindow()     bring the window to the front (notification click)
 *   onRetry(), onOpenLogs(), onRestoreDefault()   buttons of the start / error page
 *   log
 */
function registerIpc(ctx) {
  const notifications = new Set(); // keep references until closed

  function appWindow(event) {
    const frame = event.senderFrame;
    const win = BrowserWindow.fromWebContents(event.sender);
    if (!win || !frame || frame.parent !== null || !isServerUrl(frame.url, ctx.getPort())) {
      ctx.log.warn(`rejected IPC from ${frame ? frame.url.split(/[?#]/)[0].slice(0, 80) : 'unknown frame'}`);
      throw new SenderError();
    }
    return win;
  }

  function handle(channel, validate, fn) {
    ipcMain.handle(channel, async (event, ...args) => {
      const win = appWindow(event);
      const value = validate(args);
      return fn(value, win);
    });
  }

  handle('lf:pick-files', validatePickFiles, async (opts, win) => {
    const properties = ['openFile'];
    if (opts.multiple) properties.push('multiSelections');
    const r = await dialog.showOpenDialog(win, {
      title: opts.title || ctx.t('dialog.pick_files'),
      properties,
      filters: opts.filters.length ? opts.filters : undefined,
    });
    return r.canceled ? [] : r.filePaths;
  });

  handle('lf:pick-folder', validateNoArgs, async (_, win) => {
    const r = await dialog.showOpenDialog(win, { title: ctx.t('dialog.pick_folder'), properties: ['openDirectory', 'createDirectory'] });
    return r.canceled || r.filePaths.length === 0 ? null : r.filePaths[0];
  });

  handle('lf:notify', validateNotify, async ({ title, body }) => {
    if (!Notification.isSupported()) return false;
    const n = new Notification({ title, body });
    notifications.add(n);
    const forget = () => notifications.delete(n);
    n.on('click', () => {
      forget();
      ctx.showMainWindow();
    });
    n.on('close', forget);
    n.on('failed', forget);
    n.show();
    return true;
  });

  handle('lf:power-state', validateNoArgs, async () => ({ onBattery: powerMonitor.isOnBatteryPower() }));

  handle('lf:open-external', validateOpenExternal, async (url) => {
    await shell.openExternal(url);
    return true;
  });

  handle('lf:app-version', validateNoArgs, async () => app.getVersion());

  handle('lf:components-import-file', validateNoArgs, async (_, win) => {
    const r = await dialog.showOpenDialog(win, {
      title: ctx.t('dialog.import_component'),
      properties: ['openFile'],
      filters: [
        { name: ctx.t('dialog.component_filter'), extensions: COMPONENT_EXTENSIONS },
        { name: ctx.t('dialog.all_files'), extensions: ['*'] },
      ],
    });
    return r.canceled || r.filePaths.length === 0 ? null : r.filePaths[0];
  });

  // Start / error page (file://.../splash.html only).
  function fromShellPage(event) {
    const frame = event.senderFrame;
    return !!frame && frame.parent === null && isShellFileUrl(frame.url, ctx.shellRoot) && !!BrowserWindow.fromWebContents(event.sender);
  }
  ipcMain.on('lf-shell:retry', (event, ...args) => {
    if (!fromShellPage(event) || args.length !== 0) return;
    ctx.onRetry();
  });
  ipcMain.on('lf-shell:open-logs', (event, ...args) => {
    if (!fromShellPage(event) || args.length !== 0) return;
    ctx.onOpenLogs();
  });
  ipcMain.on('lf-shell:restore-default', (event, ...args) => {
    if (!fromShellPage(event) || args.length !== 0) return;
    ctx.onRestoreDefault();
  });
}

module.exports = { registerIpc, COMPONENT_EXTENSIONS };
