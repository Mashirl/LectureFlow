// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// A stable port for the service. The window's storage (localStorage) is
// kept per origin, and the origin includes the port, so the port that worked
// last time is remembered in {userData}/port.json and tried first; when it
// is taken, a random free port is used and remembered instead.
'use strict';

const fs = require('node:fs');
const path = require('node:path');

const PORT_FILE = 'port.json';

function validPort(p) {
  return Number.isInteger(p) && p >= 1024 && p <= 65535;
}

/** readPortMemo(file) → the remembered port, or null. */
function readPortMemo(file, readFile = (f) => fs.readFileSync(f, 'utf8')) {
  try {
    const v = JSON.parse(readFile(file));
    return v && validPort(v.port) ? v.port : null;
  } catch {
    return null;
  }
}

/** writePortMemo(file, port): written to a temporary file, then renamed. */
function writePortMemo(file, port) {
  if (!validPort(port)) return false;
  try {
    fs.mkdirSync(path.dirname(file), { recursive: true });
    const tmp = `${file}.tmp`;
    fs.writeFileSync(tmp, `${JSON.stringify({ port })}\n`);
    fs.renameSync(tmp, file);
    return true;
  } catch {
    return false;
  }
}

/**
 * makePortPicker({ preferred, isFree, pickFree }) → async () => port.
 * The first call returns the preferred port when it is free; every other
 * call (and the first when it is taken) a random free port.
 */
function makePortPicker({ preferred, isFree, pickFree }) {
  let first = true;
  return async () => {
    if (first) {
      first = false;
      if (validPort(preferred) && (await isFree(preferred))) return preferred;
    }
    return pickFree();
  };
}

module.exports = { PORT_FILE, readPortMemo, writePortMemo, makePortPicker, validPort };
