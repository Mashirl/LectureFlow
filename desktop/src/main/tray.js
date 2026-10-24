// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Notification-area icon while the window is hidden and tasks continue in
// the background (Windows; macOS keeps the app in the Dock instead).
'use strict';

const { Menu, Tray } = require('electron');

class TrayController {
  constructor({ t, icon, onOpen, onQuit }) {
    this.t = t;
    this.icon = icon;
    this.onOpen = onOpen;
    this.onQuit = onQuit;
    this.tray = null;
    this.running = 0;
  }

  get visible() {
    return this.tray !== null;
  }

  show() {
    if (this.tray) return;
    this.tray = new Tray(this.icon);
    this.tray.setContextMenu(Menu.buildFromTemplate([
      { label: this.t('tray.open'), click: () => this.onOpen() },
      { type: 'separator' },
      { label: this.t('tray.quit'), click: () => this.onQuit() },
    ]));
    this.tray.on('click', () => this.onOpen());
    this._tooltip();
  }

  setRunning(n) {
    this.running = n;
    this._tooltip();
  }

  _tooltip() {
    if (!this.tray) return;
    this.tray.setToolTip(this.running > 0 ? this.t('tray.tooltip_running', { n: this.running }) : this.t('tray.tooltip'));
  }

  destroy() {
    if (!this.tray) return;
    this.tray.destroy();
    this.tray = null;
  }
}

module.exports = { TrayController };
