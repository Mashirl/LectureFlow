// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Start screen and error page. The main process passes the state and the
// (already translated) texts in the query string; everything is set with
// textContent. The buttons call window.lectureflowShell, which the preload
// exposes on this page only.
'use strict';

(function () {
  const q = new URLSearchParams(location.search);
  const get = (k) => (q.get(k) || '').slice(0, 4000);
  const $ = (id) => document.getElementById(id);
  const states = ['starting', 'restarting', 'upgrading', 'stopping', 'error'];
  const state = states.includes(q.get('state')) ? q.get('state') : 'starting';

  document.documentElement.lang = get('lang') || 'en';
  document.body.dataset.state = state;
  $('status').textContent = get('status');
  if (state !== 'error') return;

  const title = get('title');
  $('title').textContent = title;
  if (title) document.title = `${title} - LectureFlow`;
  $('body').textContent = get('body');
  const detail = get('detail');
  if (detail) {
    $('detail-label').textContent = get('detailLabel');
    $('detail').textContent = detail;
  } else {
    $('detail-wrap').hidden = true;
  }
  const retry = $('retry');
  const logs = $('logs');
  retry.textContent = get('retry');
  logs.textContent = get('logs');
  const shell = window.lectureflowShell;
  const restore = $('restore');
  if (get('restore')) {
    restore.textContent = get('restore');
    restore.hidden = false;
    restore.addEventListener('click', () => {
      if (shell) shell.restoreDefault();
    });
  }
  retry.addEventListener('click', () => {
    retry.disabled = true;
    logs.disabled = true;
    if (shell) shell.retry();
  });
  logs.addEventListener('click', () => {
    if (shell) shell.openLogs();
  });
  retry.focus();
})();
