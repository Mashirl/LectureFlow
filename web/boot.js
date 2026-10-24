// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Loaded synchronously from <head> (a classic script, not a module) so it
// runs before the first paint; it was the inline <script> of index.html and
// moved here for the Content-Security-Policy script-src 'self' (SPEC-v3 V2).
// Before the first paint: ?theme=dark|light forces the colour scheme (A12.1
// tokens); the shell layout (nav rail below 1280 px, inspector rail below
// 1200 px or when collapsed) is set so the page does not jump. app.js keeps
// both attributes up to date. The static text of index.html is filled in by app.js
// from the data-i18n* attributes (SPEC-v2 P14.2); i18n.js sets <html lang>.
(function (d) {
  try { var t = new URLSearchParams(location.search).get('theme'); if (t === 'dark' || t === 'light') d.dataset.theme = t; } catch (e) {}
  var w = window.innerWidth, pref = 'open';
  try { if (localStorage.getItem('lectureflow.inspector') === 'collapsed') pref = 'collapsed'; } catch (e) {}
  d.dataset.nav = w < 1280 ? 'rail' : 'full';
  d.dataset.insp = w < 1200 ? 'collapsed' : pref;
})(document.documentElement);
