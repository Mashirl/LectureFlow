// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The shell's log: {data dir}/logs/desktop.log (next to the service's
// lectureflow.log, so "Open logs" shows both). Secrets registered with
// addSecret (the access token) are replaced before anything is written.
'use strict';

const fs = require('node:fs');
const path = require('node:path');

const MAX_BYTES = 2 * 1024 * 1024;

function createLogger({ file = null, echo = false } = {}) {
  const secrets = new Set();
  let target = null;

  function open(f) {
    target = null;
    if (!f) return;
    try {
      fs.mkdirSync(path.dirname(f), { recursive: true });
      try {
        if (fs.statSync(f).size > MAX_BYTES) fs.renameSync(f, `${f}.1`);
      } catch {
        // no file yet
      }
      target = f;
    } catch {
      target = null;
    }
  }

  function write(level, msg) {
    let line = `${new Date().toISOString()} ${level} ${msg}`;
    for (const s of secrets) line = line.split(s).join('[token]');
    if (echo) (level === 'ERROR' ? console.error : console.log)(line);
    if (!target) return;
    try {
      fs.appendFileSync(target, `${line}\n`);
    } catch {
      // logging must never take the app down
    }
  }

  open(file);
  return {
    info: (m) => write('INFO', m),
    warn: (m) => write('WARN', m),
    error: (m) => write('ERROR', m),
    addSecret: (s) => {
      if (s) secrets.add(s);
    },
    removeSecret: (s) => secrets.delete(s),
    setFile: (f) => open(f),
    get file() {
      return target;
    },
  };
}

module.exports = { createLogger };
