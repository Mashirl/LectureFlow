// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Who may talk to the background over a port, and what an in-page port is
// told (security review L3). A port's name is chosen by whoever connects,
// so it is checked against the sender the browser reports: "popup",
// "options" and "confirm" only from the extension's own pages of those
// names, "inpage" only from a content script in a web page (a tab, an
// http(s) frame). An in-page port hears only about its own platform's
// tasks, only for the sessions that page asked about or sent, and never
// which course they went to. Pure: the background passes the sender and
// the extension's page URLs in.

const OWN_PAGES = { popup: 'popup/popup.html', options: 'options/options.html', confirm: 'confirm/confirm.html' };

function httpUrl(raw) {
  try {
    const u = new URL(String(raw || ''));
    return u.protocol === 'http:' || u.protocol === 'https:' ? u : null;
  } catch {
    return null;
  }
}

/**
 * The kind of a port ('popup' | 'options' | 'confirm' | 'inpage'), or ''
 * to refuse it. sender: port.sender; extensionId: runtime.id; pageUrl(rel):
 * runtime.getURL(rel); frameUrl: the sender's frame address (sender.url, or
 * the tab's for the top frame).
 */
export function portKindOf(name, sender, { extensionId, pageUrl, frameUrl = '' }) {
  if (!sender || sender.id !== extensionId) return '';
  if (Object.prototype.hasOwnProperty.call(OWN_PAGES, name)) {
    try {
      const u = new URL(String(sender.url || ''));
      const want = new URL(pageUrl(OWN_PAGES[name]));
      return u.origin === want.origin && u.pathname === want.pathname ? name : '';
    } catch {
      return '';
    }
  }
  if (name === 'inpage') {
    const tab = sender.tab;
    return tab && Number.isInteger(tab.id) && tab.id >= 0 && httpUrl(frameUrl) ? 'inpage' : '';
  }
  return '';
}

/**
 * A status push as an in-page port may see it, or null: its platform's
 * tasks only, only sessions in `interest`, without the course.
 */
export function statusForPage(status, { platform, interest }) {
  if (!status || typeof status !== 'object' || !platform || status.platform !== platform) return null;
  if (!interest || !interest.has(String(status.sessionId || ''))) return null;
  const { courseId: _c, ...rest } = status;
  return rest;
}
