// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Store packages from the builds: dist/lectureflow-<target>-<version>.zip.
// Deterministic (files in sorted order, stored without compression, a fixed
// 1980-01-01 timestamp), so the same source gives the same bytes on any
// machine. The Chrome package leaves out the development "key": the stores
// assign their own extension ids.
//
//   node scripts/build.mjs && node scripts/package.mjs [--target chrome|firefox|all]

import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { crc32 } from './lib.mjs';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');

function files(dir, base = dir) {
  const out = [];
  for (const e of fs.readdirSync(dir, { withFileTypes: true })) {
    const p = path.join(dir, e.name);
    if (e.isDirectory()) out.push(...files(p, base));
    else out.push(path.relative(base, p).split(path.sep).join('/'));
  }
  return out.sort();
}

/** A stored zip of [{ name, data }]. */
export function zip(entries) {
  const DOS_TIME = 0; // 00:00:00
  const DOS_DATE = (0 << 9) | (1 << 5) | 1; // 1980-01-01
  const locals = [];
  const centrals = [];
  let offset = 0;
  for (const { name, data } of entries) {
    const nameBuf = Buffer.from(name, 'utf8');
    const crc = crc32(data);
    const local = Buffer.alloc(30);
    local.writeUInt32LE(0x04034b50, 0);
    local.writeUInt16LE(20, 4); // version needed
    local.writeUInt16LE(0x0800, 6); // UTF-8 names
    local.writeUInt16LE(0, 8); // stored
    local.writeUInt16LE(DOS_TIME, 10);
    local.writeUInt16LE(DOS_DATE, 12);
    local.writeUInt32LE(crc, 14);
    local.writeUInt32LE(data.length, 18);
    local.writeUInt32LE(data.length, 22);
    local.writeUInt16LE(nameBuf.length, 26);
    local.writeUInt16LE(0, 28);
    locals.push(local, nameBuf, data);
    const central = Buffer.alloc(46);
    central.writeUInt32LE(0x02014b50, 0);
    central.writeUInt16LE(20, 4); // made by
    central.writeUInt16LE(20, 6);
    central.writeUInt16LE(0x0800, 8);
    central.writeUInt16LE(0, 10);
    central.writeUInt16LE(DOS_TIME, 12);
    central.writeUInt16LE(DOS_DATE, 14);
    central.writeUInt32LE(crc, 16);
    central.writeUInt32LE(data.length, 20);
    central.writeUInt32LE(data.length, 24);
    central.writeUInt16LE(nameBuf.length, 28);
    central.writeUInt32LE(offset, 42);
    centrals.push(central, nameBuf);
    offset += 30 + nameBuf.length + data.length;
  }
  const cd = Buffer.concat(centrals);
  const end = Buffer.alloc(22);
  end.writeUInt32LE(0x06054b50, 0);
  end.writeUInt16LE(entries.length, 8);
  end.writeUInt16LE(entries.length, 10);
  end.writeUInt32LE(cd.length, 12);
  end.writeUInt32LE(offset, 16);
  return Buffer.concat([...locals, cd, end]);
}

export function packageTarget(target) {
  const dir = path.join(root, 'dist', target);
  if (!fs.existsSync(path.join(dir, 'manifest.json'))) throw new Error(`build ${target} first`);
  const entries = files(dir).map((name) => {
    let data = fs.readFileSync(path.join(dir, name));
    if (name === 'manifest.json' && target === 'chrome') {
      const m = JSON.parse(data.toString('utf8'));
      delete m.key;
      data = Buffer.from(JSON.stringify(m, null, 2) + '\n');
    }
    return { name, data };
  });
  const version = JSON.parse(fs.readFileSync(path.join(dir, 'manifest.json'), 'utf8')).version;
  const out = path.join(root, 'dist', `lectureflow-${target}-${version}.zip`);
  fs.writeFileSync(out, zip(entries));
  return out;
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const i = process.argv.indexOf('--target');
  const t = i >= 0 ? process.argv[i + 1] : 'all';
  for (const target of t === 'all' ? ['chrome', 'firefox'] : [t]) console.log(`packaged ${path.relative(root, packageTarget(target))}`);
}
