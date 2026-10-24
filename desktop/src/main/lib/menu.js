// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Menus and keyboard shortcuts.
//
// Windows (and Linux): no menu at all; the web app has its own navigation.
// Copy and paste are handled by the page itself; zoom (Ctrl + = / - / 0)
// and, in development, reload and DevTools come from shortcutFor() via
// webContents 'before-input-event'.
// macOS: the application menu (About, Settings, Quit) is filled in at step 7;
// the structure is here so that step only adds items. Its roles carry the
// usual shortcuts, so shortcutFor() leaves macOS alone.
'use strict';

function viewSubmenu(isDev) {
  const items = [{ role: 'resetZoom' }, { role: 'zoomIn' }, { role: 'zoomOut' }, { type: 'separator' }, { role: 'togglefullscreen' }];
  if (isDev) items.push({ type: 'separator' }, { role: 'reload' }, { role: 'forceReload' }, { role: 'toggleDevTools' });
  return items;
}

/**
 * buildMenuTemplate({ platform, isDev, appName, t, onOpenSettings }) →
 * template for Menu.buildFromTemplate, or null (no application menu).
 * onOpenSettings is used by the macOS application menu (step 7).
 */
function buildMenuTemplate({ platform, isDev, appName = 'LectureFlow', t = (k) => k, onOpenSettings }) {
  if (platform !== 'darwin') return null;
  const appMenu = [{ role: 'about' }, { type: 'separator' }];
  if (typeof onOpenSettings === 'function') {
    appMenu.push({ id: 'settings', label: t('menu.settings'), accelerator: 'Cmd+,', click: onOpenSettings }, { type: 'separator' });
  }
  appMenu.push({ role: 'hide' }, { role: 'hideOthers' }, { role: 'unhide' }, { type: 'separator' }, { role: 'quit' });
  return [
    { id: 'app', label: appName, submenu: appMenu },
    { id: 'edit', role: 'editMenu' },
    { id: 'view', label: t('menu.view'), submenu: viewSubmenu(isDev) },
    { id: 'window', role: 'windowMenu' },
  ];
}

/**
 * shortcutFor(input, { platform, isDev }) maps a 'before-input-event' input
 * to 'zoomIn' | 'zoomOut' | 'zoomReset' | 'reload' | 'devTools' | null.
 */
function shortcutFor(input, { platform, isDev }) {
  if (platform === 'darwin' || !input || input.type !== 'keyDown') return null;
  if (!input.control || input.alt || input.meta) return null;
  const key = String(input.key);
  const code = String(input.code || '');
  if (!input.shift || key === '+') {
    if (key === '=' || key === '+' || code === 'NumpadAdd') return 'zoomIn';
    if (key === '-' || code === 'NumpadSubtract') return 'zoomOut';
    if (key === '0' || code === 'Numpad0') return 'zoomReset';
  }
  if (isDev && !input.shift && key.toLowerCase() === 'r') return 'reload';
  if (isDev && input.shift && key.toLowerCase() === 'i') return 'devTools';
  return null;
}

const ZOOM_STEP = 0.5;
const ZOOM_MIN = -3;
const ZOOM_MAX = 4;

/** nextZoomLevel(level, action) → the new Chromium zoom level. */
function nextZoomLevel(level, action) {
  const l = Number.isFinite(level) ? level : 0;
  if (action === 'zoomReset') return 0;
  const next = action === 'zoomIn' ? l + ZOOM_STEP : action === 'zoomOut' ? l - ZOOM_STEP : l;
  return Math.min(ZOOM_MAX, Math.max(ZOOM_MIN, next));
}

module.exports = { buildMenuTemplate, shortcutFor, nextZoomLevel };
