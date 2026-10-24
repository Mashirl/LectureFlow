// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// A placeholder app mark (blue rounded square with four white level bars,
// like the web app's brand mark) drawn in code and encoded as PNG, so the
// tray and the window have an icon before step 8 adds the real artwork.
'use strict';

const zlib = require('node:zlib');

const ACCENT = [0x1f, 0x6f, 0xeb]; // --accent (light) in web/style.css
// Bars as fractions of the size: [x centre, half height].
const BARS = [[0.30, 0.13], [0.43, 0.24], [0.57, 0.18], [0.70, 0.09]];
const SAMPLES = 4; // 4x4 supersampling for smooth edges

function insideRoundedSquare(x, y, size) {
  const r = size * 0.22;
  const cx = Math.min(Math.max(x, r), size - r);
  const cy = Math.min(Math.max(y, r), size - r);
  return (x - cx) ** 2 + (y - cy) ** 2 <= r * r;
}

function insideBar(x, y, size) {
  const w = size * 0.075;
  for (const [fx, fh] of BARS) {
    const cx = fx * size;
    const half = fh * size;
    const top = size / 2 - half;
    const bottom = size / 2 + half;
    // Capsule: rectangle with round ends.
    const yy = Math.min(Math.max(y, top + w / 2), bottom - w / 2);
    if ((x - cx) ** 2 + (y - yy) ** 2 <= (w / 2) ** 2) return true;
  }
  return false;
}

/** renderRgba(size) → Buffer of size*size*4 (straight RGBA). */
function renderRgba(size) {
  const buf = Buffer.alloc(size * size * 4);
  for (let py = 0; py < size; py++) {
    for (let px = 0; px < size; px++) {
      let cover = 0;
      let white = 0;
      for (let sy = 0; sy < SAMPLES; sy++) {
        for (let sx = 0; sx < SAMPLES; sx++) {
          const x = px + (sx + 0.5) / SAMPLES;
          const y = py + (sy + 0.5) / SAMPLES;
          if (insideRoundedSquare(x, y, size)) {
            cover++;
            if (insideBar(x, y, size)) white++;
          }
        }
      }
      const n = SAMPLES * SAMPLES;
      const i = (py * size + px) * 4;
      if (cover === 0) continue;
      const wf = white / cover;
      buf[i] = Math.round(ACCENT[0] + (255 - ACCENT[0]) * wf);
      buf[i + 1] = Math.round(ACCENT[1] + (255 - ACCENT[1]) * wf);
      buf[i + 2] = Math.round(ACCENT[2] + (255 - ACCENT[2]) * wf);
      buf[i + 3] = Math.round((255 * cover) / n);
    }
  }
  return buf;
}

function chunk(type, data) {
  const len = Buffer.alloc(4);
  len.writeUInt32BE(data.length);
  const td = Buffer.concat([Buffer.from(type, 'ascii'), data]);
  const crc = Buffer.alloc(4);
  crc.writeUInt32BE(zlib.crc32(td) >>> 0);
  return Buffer.concat([len, td, crc]);
}

/** encodePng(rgba, size) → PNG bytes (8-bit RGBA, no filtering). */
function encodePng(rgba, size) {
  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(size, 0);
  ihdr.writeUInt32BE(size, 4);
  ihdr[8] = 8; // bit depth
  ihdr[9] = 6; // colour type RGBA
  const raw = Buffer.alloc(size * (size * 4 + 1));
  for (let y = 0; y < size; y++) {
    raw[y * (size * 4 + 1)] = 0; // filter: none
    rgba.copy(raw, y * (size * 4 + 1) + 1, y * size * 4, (y + 1) * size * 4);
  }
  return Buffer.concat([
    Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
    chunk('IHDR', ihdr),
    chunk('IDAT', zlib.deflateSync(raw)),
    chunk('IEND', Buffer.alloc(0)),
  ]);
}

/** iconPng(size) → PNG of the app mark at size × size pixels. */
function iconPng(size) {
  if (!Number.isInteger(size) || size < 8 || size > 1024) throw new RangeError(`icon size ${size}`);
  return encodePng(renderRgba(size), size);
}

module.exports = { iconPng, renderRgba, encodePng };
