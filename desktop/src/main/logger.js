// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The shell's log: {data dir}/logs/desktop.log (next to the service's
// lectureflow.log, so "Open logs" shows both). Secrets registered with
// addSecret (the access token) are replaced before anything is written.
//
// The logger creates the log's own folder (logs/) but never the folders
// above it (security review sec-update M2, datadir-ui): a data dir that does
// not exist yet is created by the service, private from the start
// (internal/dirperm), while a folder made here would get the default
// permissions of where it lies (on D:\, "Authenticated Users: modify").
// Until it exists the shell logs into its profile (shellLogFile).
'use strict';

const fs = require('node:fs');
const path = require('node:path');

const MAX_BYTES = 2 * 1024 * 1024;
const LOG_DIR = 'logs';
const LOG_NAME = 'desktop.log';

/**
 * shellLogFile: where the shell logs — {data dir}/logs/desktop.log once the
 * data dir exists, until then {profile}/logs/desktop.log (the profile is
 * Electron's userData, which Electron has created). isDir(p) → boolean.
 */
function shellLogFile({ dataDir, profileDir, isDir, pathMod = path }) {
  if (dataDir && isDir(dataDir)) return pathMod.join(dataDir, LOG_DIR, LOG_NAME);
  return pathMod.join(profileDir, LOG_DIR, LOG_NAME);
}

// mkdirLeaf creates dir itself when its parent exists (never the parent).
function mkdirLeaf(dir) {
  try {
    fs.mkdirSync(dir);
  } catch (err) {
    if (!err || err.code !== 'EEXIST') throw err;
  }
}

function createLogger({ file = null, echo = false } = {}) {
  const secrets = new Set();
  let target = null;

  function open(f) {
    target = null;
    if (!f) return;
    try {
      mkdirLeaf(path.dirname(f));
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

module.exports = { createLogger, shellLogFile, mkdirLeaf };
