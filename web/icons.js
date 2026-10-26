// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// Inline SVG icons (v1.1 A12.1). The path data is copied from Lucide
// (https://lucide.dev); 24×24 viewBox, stroked. See NOTES.md.
//
// ISC License
// Copyright (c) for portions of Lucide are held by Cole Bemis 2013-2022 as
// part of Feather (MIT). All other copyright (c) for Lucide are held by
// Lucide Contributors 2022.
// Permission to use, copy, modify, and/or distribute this software for any
// purpose with or without fee is hereby granted, provided that the above
// copyright notice and this permission notice appear in all copies.
// THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
// WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
// MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
// ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
// WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
// ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF OR
// IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.

const SVG_NS = 'http://www.w3.org/2000/svg';

// Each icon is a list of [tag, attrs] children of the <svg>.
const P = (d) => ['path', { d }];
const ICONS = {
  // phase 3b (transcription devices): order buttons and battery mode
  'arrow-up': [P('m5 12 7-7 7 7'), P('M12 19V5')],
  'arrow-down': [P('M12 5v14'), P('m19 12-7 7-7-7')],
  'battery-medium': [
    ['rect', { width: 16, height: 10, x: 2, y: 7, rx: 2, ry: 2 }],
    ['line', { x1: 22, x2: 22, y1: 11, y2: 13 }],
    ['line', { x1: 6, x2: 6, y1: 11, y2: 13 }],
    ['line', { x1: 10, x2: 10, y1: 11, y2: 13 }],
  ],
  // power management: the resource monitor's power row
  'battery-low': [
    ['rect', { width: 16, height: 10, x: 2, y: 7, rx: 2, ry: 2 }],
    ['line', { x1: 22, x2: 22, y1: 11, y2: 13 }],
    ['line', { x1: 6, x2: 6, y1: 11, y2: 13 }],
  ],
  'battery-full': [
    ['rect', { width: 16, height: 10, x: 2, y: 7, rx: 2, ry: 2 }],
    ['line', { x1: 22, x2: 22, y1: 11, y2: 13 }],
    ['line', { x1: 6, x2: 6, y1: 11, y2: 13 }],
    ['line', { x1: 10, x2: 10, y1: 11, y2: 13 }],
    ['line', { x1: 14, x2: 14, y1: 11, y2: 13 }],
  ],
  'battery-charging': [P('M15 7h1a2 2 0 0 1 2 2v6a2 2 0 0 1-2 2h-2'), P('M6 7H4a2 2 0 0 0-2 2v6a2 2 0 0 0 2 2h1'), P('m11 7-3 5h4l-3 5'), ['line', { x1: 22, x2: 22, y1: 11, y2: 13 }]],
  'audio-lines': [P('M2 10v3'), P('M6 6v11'), P('M10 3v18'), P('M14 8v7'), P('M18 5v13'), P('M22 10v3')],
  'layout-dashboard': [
    ['rect', { width: 7, height: 9, x: 3, y: 3, rx: 1 }],
    ['rect', { width: 7, height: 5, x: 14, y: 3, rx: 1 }],
    ['rect', { width: 7, height: 9, x: 14, y: 12, rx: 1 }],
    ['rect', { width: 7, height: 5, x: 3, y: 16, rx: 1 }],
  ],
  library: [P('m16 6 4 14'), P('M12 6v14'), P('M8 8v12'), P('M4 4v16')],
  settings: [
    P('M12.22 2h-.44a2 2 0 0 0-2 2v.18a2 2 0 0 1-1 1.73l-.43.25a2 2 0 0 1-2 0l-.15-.08a2 2 0 0 0-2.73.73l-.22.38a2 2 0 0 0 .73 2.73l.15.1a2 2 0 0 1 1 1.72v.51a2 2 0 0 1-1 1.74l-.15.09a2 2 0 0 0-.73 2.73l.22.38a2 2 0 0 0 2.73.73l.15-.08a2 2 0 0 1 2 0l.43.25a2 2 0 0 1 1 1.73V20a2 2 0 0 0 2 2h.44a2 2 0 0 0 2-2v-.18a2 2 0 0 1 1-1.73l.43-.25a2 2 0 0 1 2 0l.15.08a2 2 0 0 0 2.73-.73l.22-.39a2 2 0 0 0-.73-2.73l-.15-.08a2 2 0 0 1-1-1.74v-.5a2 2 0 0 1 1-1.74l.15-.09a2 2 0 0 0 .73-2.73l-.22-.38a2 2 0 0 0-2.73-.73l-.15.08a2 2 0 0 1-2 0l-.43-.25a2 2 0 0 1-1-1.73V4a2 2 0 0 0-2-2z'),
    ['circle', { cx: 12, cy: 12, r: 3 }],
  ],
  stethoscope: [
    P('M11 2v2'),
    P('M5 2v2'),
    P('M5 3H4a2 2 0 0 0-2 2v4a6 6 0 0 0 12 0V5a2 2 0 0 0-2-2h-1'),
    P('M8 15a6 6 0 0 0 12 0v-3'),
    ['circle', { cx: 20, cy: 10, r: 2 }],
  ],
  bell: [P('M10.268 21a2 2 0 0 0 3.464 0'), P('M3.262 15.326A1 1 0 0 0 4 17h16a1 1 0 0 0 .74-1.673C19.41 13.956 18 12.499 18 8A6 6 0 0 0 6 8c0 4.499-1.411 5.956-2.738 7.326')],
  'bell-off': [
    P('M10.268 21a2 2 0 0 0 3.464 0'),
    P('M17 17H4a1 1 0 0 1-.74-1.673C4.59 13.956 6 12.499 6 8a6 6 0 0 1 .258-1.742'),
    P('m2 2 20 20'),
    P('M8.668 3.01A6 6 0 0 1 18 8c0 2.687.77 4.653 1.707 6.05'),
  ],
  'panel-right-close': [['rect', { width: 18, height: 18, x: 3, y: 3, rx: 2 }], P('M15 3v18'), P('m8 9 3 3-3 3')],
  'panel-right-open': [['rect', { width: 18, height: 18, x: 3, y: 3, rx: 2 }], P('M15 3v18'), P('m10 15-3-3 3-3')],
  'chevron-right': [P('m9 18 6-6-6-6')],
  'chevron-down': [P('m6 9 6 6 6-6')],
  ellipsis: [['circle', { cx: 12, cy: 12, r: 1 }], ['circle', { cx: 19, cy: 12, r: 1 }], ['circle', { cx: 5, cy: 12, r: 1 }]],
  download: [P('M12 15V3'), P('M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4'), P('m7 10 5 5 5-5')],
  upload: [P('M12 3v12'), P('m17 8-5-5-5 5'), P('M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4')],
  play: [P('M6 3l14 9-14 9V3z')],
  pause: [['rect', { x: 14, y: 4, width: 4, height: 16, rx: 1 }], ['rect', { x: 6, y: 4, width: 4, height: 16, rx: 1 }]],
  ban: [['circle', { cx: 12, cy: 12, r: 10 }], P('m4.9 4.9 14.2 14.2')],
  'rotate-cw': [P('M21 12a9 9 0 1 1-9-9c2.52 0 4.93 1 6.74 2.74L21 8'), P('M21 3v5h-5')],
  'refresh-cw': [P('M3 12a9 9 0 0 1 9-9 9.75 9.75 0 0 1 6.74 2.74L21 8'), P('M21 3v5h-5'), P('M21 12a9 9 0 0 1-9 9 9.75 9.75 0 0 1-6.74-2.74L3 16'), P('M8 16H3v5')],
  'trash-2': [P('M3 6h18'), P('M19 6v14c0 1-1 2-2 2H7c-1 0-2-1-2-2V6'), P('M8 6V4c0-1 1-2 2-2h4c1 0 2 1 2 2v2'), P('M10 11v6'), P('M14 11v6')],
  x: [P('M18 6 6 18'), P('m6 6 12 12')],
  check: [P('M20 6 9 17l-5-5')],
  plus: [P('M5 12h14'), P('M12 5v14')],
  'file-text': [P('M15 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V7Z'), P('M14 2v4a2 2 0 0 0 2 2h4'), P('M10 9H8'), P('M16 13H8'), P('M16 17H8')],
  cpu: [
    ['rect', { width: 16, height: 16, x: 4, y: 4, rx: 2 }],
    ['rect', { width: 6, height: 6, x: 9, y: 9, rx: 1 }],
    P('M15 2v2'), P('M15 20v2'), P('M2 15h2'), P('M2 9h2'), P('M20 15h2'), P('M20 9h2'), P('M9 2v2'), P('M9 20v2'),
  ],
  thermometer: [P('M14 4v10.54a4 4 0 1 1-4 0V4a2 2 0 0 1 4 0Z')],
  zap: [P('M13 2 3 14h9l-1 8 10-12h-9l1-8z')],
  gauge: [P('m12 14 4-4'), P('M3.34 19a10 10 0 1 1 17.32 0')],
  activity: [P('M22 12h-4l-3 9L9 3l-3 9H2')],
  'circle-alert': [['circle', { cx: 12, cy: 12, r: 10 }], P('M12 8v4'), P('M12 16h.01')],
  'triangle-alert': [P('m21.73 18-8-14a2 2 0 0 0-3.48 0l-8 14A2 2 0 0 0 4 21h16a2 2 0 0 0 1.73-3'), P('M12 9v4'), P('M12 17h.01')],
  'circle-check': [['circle', { cx: 12, cy: 12, r: 10 }], P('m9 12 2 2 4-4')],
  clock: [['circle', { cx: 12, cy: 12, r: 10 }], P('M12 6v6l4 2')],
  search: [['circle', { cx: 11, cy: 11, r: 8 }], P('m21 21-4.3-4.3')],
  pencil: [P('M21.174 6.812a1 1 0 0 0-3.986-3.987L3.842 16.174a2 2 0 0 0-.5.83l-1.321 4.352a.5.5 0 0 0 .623.622l4.353-1.32a2 2 0 0 0 .83-.497z'), P('m15 5 4 4')],
  hash: [P('M4 9h16'), P('M4 15h16'), P('M10 3 8 21'), P('M16 3l-2 18')],
  'external-link': [P('M15 3h6v6'), P('M10 14 21 3'), P('M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6')],
  'graduation-cap': [P('M21.42 10.922a1 1 0 0 0-.019-1.838L12.83 5.18a2 2 0 0 0-1.66 0L2.6 9.08a1 1 0 0 0 0 1.832l8.57 3.908a2 2 0 0 0 1.66 0z'), P('M22 10v6'), P('M6 12.5V16a6 3 0 0 0 12 0v-3.5')],
  wallet: [P('M19 7V4a1 1 0 0 0-1-1H5a2 2 0 0 0 0 4h15a1 1 0 0 1 1 1v4h-3a2 2 0 0 0 0 4h3a1 1 0 0 0 1-1v-2a1 1 0 0 0-1-1'), P('M3 5v14a2 2 0 0 0 2 2h15a1 1 0 0 0 1-1v-4')],
  'list-checks': [P('m3 17 2 2 4-4'), P('m3 7 2 2 4-4'), P('M13 6h8'), P('M13 12h8'), P('M13 18h8')],
  inbox: [P('M22 12h-6l-2 3h-4l-2-3H2'), P('M5.45 5.11 2 12v6a2 2 0 0 0 2 2h16a2 2 0 0 0 2-2v-6l-3.45-6.89A2 2 0 0 0 16.76 4H7.24a2 2 0 0 0-1.79 1.11z')],
  copy: [['rect', { width: 14, height: 14, x: 8, y: 8, rx: 2, ry: 2 }], P('M4 16c-1.1 0-2-.9-2-2V4c0-1.1.9-2 2-2h10c1.1 0 2 .9 2 2')],
  braces: [P('M8 3H7a2 2 0 0 0-2 2v5a2 2 0 0 1-2 2 2 2 0 0 1 2 2v5c0 1.1.9 2 2 2h1'), P('M16 21h1a2 2 0 0 0 2-2v-5c0-1.1.9-2 2-2a2 2 0 0 1-2-2V5a2 2 0 0 0-2-2h-1')],
  captions: [['rect', { width: 18, height: 14, x: 3, y: 5, rx: 2, ry: 2 }], P('M7 15h4M15 15h2M7 11h2M13 11h4')],
  'arrow-left': [P('m12 19-7-7 7-7'), P('M19 12H5')],
  'loader-circle': [P('M21 12a9 9 0 1 1-6.219-8.56')],
  layers: [
    P('m12.83 2.18a2 2 0 0 0-1.66 0L2.6 6.08a1 1 0 0 0 0 1.83l8.58 3.91a2 2 0 0 0 1.66 0l8.58-3.9a1 1 0 0 0 0-1.83Z'),
    P('m22 17.65-9.17 4.16a2 2 0 0 1-1.66 0L2 17.65'),
    P('m22 12.65-9.17 4.16a2 2 0 0 1-1.66 0L2 12.65'),
  ],
  'memory-stick': [P('M6 19v-3'), P('M10 19v-3'), P('M14 19v-3'), P('M18 19v-3'), P('M8 11V9'), P('M16 11V9'), P('M12 11V9'), P('M2 15h20'), P('M2 7a2 2 0 0 1 2-2h16a2 2 0 0 1 2 2v1.1a2 2 0 0 0 0 3.837V17a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2v-5.1a2 2 0 0 0 0-3.837Z')],
  'circle-pause': [['circle', { cx: 12, cy: 12, r: 10 }], P('M10 15V9'), P('M14 15V9')],
  sparkles: [
    P('M9.937 15.5A2 2 0 0 0 8.5 14.063l-6.135-1.582a.5.5 0 0 1 0-.962L8.5 9.936A2 2 0 0 0 9.937 8.5l1.582-6.135a.5.5 0 0 1 .963 0L14.063 8.5A2 2 0 0 0 15.5 9.937l6.135 1.581a.5.5 0 0 1 0 .964L15.5 14.063a2 2 0 0 0-1.437 1.437l-1.582 6.135a.5.5 0 0 1-.963 0z'),
  ],
  'book-open': [P('M12 7v14'), P('M3 18a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1h5a4 4 0 0 1 4 4 4 4 0 0 1 4-4h5a1 1 0 0 1 1 1v13a1 1 0 0 1-1 1h-6a3 3 0 0 0-3 3 3 3 0 0 0-3-3z')],
  // F-lecture (lecture page, transcript menu, review)
  info: [['circle', { cx: 12, cy: 12, r: 10 }], P('M12 16v-4'), P('M12 8h.01')],
  'check-check': [P('M18 6 7 17l-5-5'), P('m22 10-7.5 7.5L13 16')],
  quote: [
    P('M16 3a2 2 0 0 0-2 2v6a2 2 0 0 0 2 2 1 1 0 0 1 1 1v1a2 2 0 0 1-2 2 1 1 0 0 0-1 1v2a1 1 0 0 0 1 1 6 6 0 0 0 6-6V5a2 2 0 0 0-2-2z'),
    P('M5 3a2 2 0 0 0-2 2v6a2 2 0 0 0 2 2 1 1 0 0 1 1 1v1a2 2 0 0 1-2 2 1 1 0 0 0-1 1v2a1 1 0 0 0 1 1 6 6 0 0 0 6-6V5a2 2 0 0 0-2-2z'),
  ],
  'undo-2': [P('M9 14 4 9l5-5'), P('M4 9h10.5a5.5 5.5 0 0 1 5.5 5.5a5.5 5.5 0 0 1-5.5 5.5H11')],
  languages: [P('m5 8 6 6'), P('m4 14 6-6 2-3'), P('M2 5h12'), P('M7 2h1'), P('m22 22-5-10-5 10'), P('M14 18h6')],
  coins: [['circle', { cx: 8, cy: 8, r: 6 }], P('M18.09 10.37A6 6 0 1 1 10.34 18'), P('M7 6h1v4'), P('m16.71 13.88.7.71-2.82 2.82')],
  // F-settings (settings and diagnostics)
  server: [['rect', { width: 20, height: 8, x: 2, y: 2, rx: 2, ry: 2 }], ['rect', { width: 20, height: 8, x: 2, y: 14, rx: 2, ry: 2 }], P('M6 6h.01'), P('M6 18h.01')],
  plug: [P('M12 22v-5'), P('M9 8V2'), P('M15 8V2'), P('M18 8v5a4 4 0 0 1-4 4h-4a4 4 0 0 1-4-4V8Z')],
  'scan-search': [P('M3 7V5a2 2 0 0 1 2-2h2'), P('M17 3h2a2 2 0 0 1 2 2v2'), P('M21 17v2a2 2 0 0 1-2 2h-2'), P('M7 21H5a2 2 0 0 1-2-2v-2'), ['circle', { cx: 12, cy: 12, r: 3 }], P('m16 16-1.9-1.9')],
  'key-round': [P('M2.586 17.414A2 2 0 0 0 2 18.828V21a1 1 0 0 0 1 1h3a1 1 0 0 0 1-1v-1a1 1 0 0 1 1-1h1a1 1 0 0 0 1-1v-1a1 1 0 0 1 1-1h.172a2 2 0 0 0 1.414-.586l.814-.814a6.5 6.5 0 1 0-4-4z'), ['circle', { cx: 16.5, cy: 7.5, r: 0.5, fill: 'currentColor' }]],
  'hard-drive': [P('M22 12H2'), P('M5.45 5.11 2 12v6a2 2 0 0 0 2 2h16a2 2 0 0 0 2-2v-6l-3.45-6.89A2 2 0 0 0 16.76 4H7.24a2 2 0 0 0-1.79 1.11z'), P('M6 16h.01'), P('M10 16h.01')],
  mic: [P('M12 2a3 3 0 0 0-3 3v7a3 3 0 0 0 6 0V5a3 3 0 0 0-3-3Z'), P('M19 10v2a7 7 0 0 1-14 0v-2'), P('M12 19v3')],
  cloud: [P('M17.5 19H9a7 7 0 1 1 6.71-9h1.79a4.5 4.5 0 1 1 0 9Z')],
  'log-in': [P('m10 17 5-5-5-5'), P('M15 12H3'), P('M15 3h4a2 2 0 0 1 2 2v14a2 2 0 0 1-2 2h-4')],
  'folder-open': [P('m6 14 1.5-2.9A2 2 0 0 1 9.24 10H20a2 2 0 0 1 1.94 2.5l-1.54 6a2 2 0 0 1-1.95 1.5H4a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h3.9a2 2 0 0 1 1.69.9l.81 1.2a2 2 0 0 0 1.67.9H18a2 2 0 0 1 2 2v2')],
  bot: [P('M12 8V4H8'), ['rect', { width: 16, height: 12, x: 4, y: 8, rx: 2 }], P('M2 14h2'), P('M20 14h2'), P('M15 13v2'), P('M9 13v2')],
  'sliders-horizontal': [P('M21 4h-7'), P('M10 4H3'), P('M21 12h-9'), P('M8 12H3'), P('M21 20h-5'), P('M12 20H3'), P('M14 2v4'), P('M8 10v4'), P('M16 18v4')],
  // v2 glossary (tiers, locks, janitor)
  lock: [['rect', { width: 18, height: 11, x: 3, y: 11, rx: 2, ry: 2 }], P('M7 11V7a5 5 0 0 1 10 0v4')],
  'lock-open': [['rect', { width: 18, height: 11, x: 3, y: 11, rx: 2, ry: 2 }], P('M7 11V7a5 5 0 0 1 9.9-1')],
  'grip-vertical': [
    ['circle', { cx: 9, cy: 12, r: 1 }],
    ['circle', { cx: 9, cy: 5, r: 1 }],
    ['circle', { cx: 9, cy: 19, r: 1 }],
    ['circle', { cx: 15, cy: 12, r: 1 }],
    ['circle', { cx: 15, cy: 5, r: 1 }],
    ['circle', { cx: 15, cy: 19, r: 1 }],
  ],
  'wand-sparkles': [
    P('m21.64 3.64-1.28-1.28a1.21 1.21 0 0 0-1.72 0L2.36 18.64a1.21 1.21 0 0 0 0 1.72l1.28 1.28a1.2 1.2 0 0 0 1.72 0L21.64 5.36a1.2 1.2 0 0 0 0-1.72'),
    P('m14 7 3 3'),
    P('M5 6v4'),
    P('M19 14v4'),
    P('M10 2v2'),
    P('M7 8H3'),
    P('M21 16h-4'),
    P('M11 3H9'),
  ],
  'shield-check': [
    P('M20 13c0 5-3.5 7.5-7.66 8.95a1 1 0 0 1-.67-.01C7.5 20.5 4 18 4 13V6a1 1 0 0 1 1-1c2 0 4.5-1.2 6.24-2.72a1.17 1.17 0 0 1 1.52 0C14.51 3.81 17 5 19 5a1 1 0 0 1 1 1z'),
    P('m9 12 2 2 4-4'),
  ],
  'arrow-right': [P('M5 12h14'), P('m12 5 7 7-7 7')],
  // v2 P13 (web-course-p2)
  calendar: [P('M8 2v4'), P('M16 2v4'), ['rect', { width: 18, height: 18, x: 3, y: 4, rx: 2 }], P('M3 10h18')],
  tag: [P('M12.586 2.586A2 2 0 0 0 11.172 2H4a2 2 0 0 0-2 2v7.172a2 2 0 0 0 .586 1.414l8.704 8.704a2.426 2.426 0 0 0 3.42 0l6.58-6.58a2.426 2.426 0 0 0 0-3.42z'), ['circle', { cx: 7.5, cy: 7.5, r: 0.5, fill: 'currentColor' }]],
};

// icon(name, {size, class, label}) → <svg>. Decorative (aria-hidden) unless
// a label is given.
export function icon(name, opts = {}) {
  const defs = ICONS[name] || ICONS['circle-alert'];
  const size = opts.size || 16;
  const svg = document.createElementNS(SVG_NS, 'svg');
  svg.setAttribute('viewBox', '0 0 24 24');
  svg.setAttribute('width', String(size));
  svg.setAttribute('height', String(size));
  svg.setAttribute('fill', 'none');
  svg.setAttribute('stroke', 'currentColor');
  svg.setAttribute('stroke-width', String(opts.stroke || 2));
  svg.setAttribute('stroke-linecap', 'round');
  svg.setAttribute('stroke-linejoin', 'round');
  svg.setAttribute('class', 'icon' + (opts.class ? ' ' + opts.class : ''));
  if (opts.label) {
    svg.setAttribute('role', 'img');
    svg.setAttribute('aria-label', opts.label);
  } else {
    svg.setAttribute('aria-hidden', 'true');
    svg.setAttribute('focusable', 'false');
  }
  for (const [tag, attrs] of defs) {
    const el = document.createElementNS(SVG_NS, tag);
    for (const [k, v] of Object.entries(attrs)) el.setAttribute(k, String(v));
    svg.append(el);
  }
  return svg;
}
