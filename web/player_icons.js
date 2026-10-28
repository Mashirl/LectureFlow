// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// Icons used only by the player (SPEC-v2 P6.2). Same source, licence and
// drawing rules as icons.js (Lucide, ISC License, © Lucide Contributors; the
// licence text is in the header of icons.js); names not defined here fall
// back to icons.js.

import { icon } from './icons.js';

const SVG_NS = 'http://www.w3.org/2000/svg';
const P = (d) => ['path', { d }];
const L = (x1, y1, x2, y2) => ['line', { x1, y1, x2, y2 }];
const SPEAKER = ['polygon', { points: '11 5 6 9 2 9 2 15 6 15 11 19 11 5' }];

const ICONS = {
  // ext web3: picture in picture (second angle), swapping, chapters folded
  'picture-in-picture-2': [P('M21 9V6a2 2 0 0 0-2-2H4a2 2 0 0 0-2 2v10c0 1.1.9 2 2 2h4'), ['rect', { width: 10, height: 7, x: 12, y: 13, rx: 2 }]],
  'arrow-left-right': [P('M8 3 4 7l4 4'), P('M4 7h16'), P('m16 21 4-4-4-4'), P('M20 17H4')],
  'list-collapse': [P('m3 10 2.5-2.5L3 5'), P('m3 19 2.5-2.5L3 14'), P('M10 6h11'), P('M10 12h11'), P('M10 18h11')],
  'skip-back': [['polygon', { points: '19 20 9 12 19 4 19 20' }], L(5, 19, 5, 5)],
  'skip-forward': [['polygon', { points: '5 4 15 12 5 20 5 4' }], L(19, 5, 19, 19)],
  'repeat-1': [P('m17 2 4 4-4 4'), P('M3 11v-1a4 4 0 0 1 4-4h14'), P('m7 22-4-4 4-4'), P('M21 13v1a4 4 0 0 1-4 4H3'), P('M11 10h1v4')],
  'volume-2': [SPEAKER, P('M15.54 8.46a5 5 0 0 1 0 7.07'), P('M19.07 4.93a10 10 0 0 1 0 14.14')],
  'volume-1': [SPEAKER, P('M15.54 8.46a5 5 0 0 1 0 7.07')],
  'volume-x': [SPEAKER, L(22, 9, 16, 15), L(16, 9, 22, 15)],
  maximize: [P('M8 3H5a2 2 0 0 0-2 2v3'), P('M21 8V5a2 2 0 0 0-2-2h-3'), P('M3 16v3a2 2 0 0 0 2 2h3'), P('M16 21h3a2 2 0 0 0 2-2v-3')],
  minimize: [P('M8 3v3a2 2 0 0 1-2 2H3'), P('M21 8h-3a2 2 0 0 1-2-2V3'), P('M3 16h3a2 2 0 0 1 2 2v3'), P('M16 21v-3a2 2 0 0 1 2-2h3')],
  'rectangle-horizontal': [['rect', { width: 20, height: 12, x: 2, y: 6, rx: 2 }]],
  'panel-right': [['rect', { width: 18, height: 18, x: 3, y: 3, rx: 2 }], P('M15 3v18')],
  list: [L(8, 6, 21, 6), L(8, 12, 21, 12), L(8, 18, 21, 18), L(3, 6, 3.01, 6), L(3, 12, 3.01, 12), L(3, 18, 3.01, 18)],
  keyboard: [P('M10 8h.01'), P('M12 12h.01'), P('M14 8h.01'), P('M16 12h.01'), P('M18 8h.01'), P('M6 8h.01'), P('M7 16h10'), P('M8 12h.01'), ['rect', { width: 20, height: 16, x: 2, y: 4, rx: 2 }]],
  locate: [L(2, 12, 5, 12), L(19, 12, 22, 12), L(12, 2, 12, 5), L(12, 19, 12, 22), ['circle', { cx: 12, cy: 12, r: 7 }]],
  video: [P('m16 13 5.223 3.482a.5.5 0 0 0 .777-.416V7.87a.5.5 0 0 0-.752-.432L16 10.5'), ['rect', { x: 2, y: 6, width: 14, height: 12, rx: 2 }]],
  'rotate-ccw': [P('M3 12a9 9 0 1 0 9-9 9.75 9.75 0 0 0-6.74 2.74L3 8'), P('M3 3v5h5')],
  headphones: [P('M3 14h3a2 2 0 0 1 2 2v3a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-7a9 9 0 0 1 18 0v7a2 2 0 0 1-2 2h-1a2 2 0 0 1-2-2v-3a2 2 0 0 1 2-2h3')],
  'circle-help': [['circle', { cx: 12, cy: 12, r: 10 }], P('M9.09 9a3 3 0 0 1 5.83 1c0 2-3 3-3 3'), P('M12 17h.01')],
  'grip-horizontal': [['circle', { cx: 12, cy: 9, r: 1 }], ['circle', { cx: 19, cy: 9, r: 1 }], ['circle', { cx: 5, cy: 9, r: 1 }], ['circle', { cx: 12, cy: 15, r: 1 }], ['circle', { cx: 19, cy: 15, r: 1 }], ['circle', { cx: 5, cy: 15, r: 1 }]],
};

// pIcon(name, {size, class, label}) → <svg>, like icon() of icons.js.
export function pIcon(name, opts = {}) {
  const defs = ICONS[name];
  if (!defs) return icon(name, opts);
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
