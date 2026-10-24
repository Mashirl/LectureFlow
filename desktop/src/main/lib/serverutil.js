// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Helpers for starting the Go service (v3-brief §1). Pure functions.
'use strict';

const crypto = require('node:crypto');
const path = require('node:path');

const READY_PREFIX = 'LECTUREFLOW_READY ';
const EXIT_RESTART = 3;
const TOKEN_BYTES = 32;
const MAX_REASON_CHARS = 600;

/** generateToken: 32 random bytes as 64 hex characters (never logged, never on disk). */
function generateToken() {
  return crypto.randomBytes(TOKEN_BYTES).toString('hex');
}

/**
 * buildServerArgs: the command line of the service. The token is NOT part
 * of it (command lines are visible to other processes); it goes to stdin.
 * locationFile is omitted when the data dir came from LECTUREFLOW_DATA_DIR.
 */
function buildServerArgs({ port, dataDir, resourcesDir, appVersion, configFile, locationFile }) {
  const args = ['-desktop', '-listen', `127.0.0.1:${port}`, '-data-dir', dataDir];
  if (resourcesDir) args.push('-resources', resourcesDir);
  if (appVersion) args.push('-app-version', appVersion);
  if (configFile) args.push('-config', configFile);
  if (locationFile) args.push('-location-file', locationFile);
  return args;
}

/** parseReadyLine: "LECTUREFLOW_READY http://127.0.0.1:PORT" → { url, port } or null. */
function parseReadyLine(line) {
  if (typeof line !== 'string' || !line.startsWith(READY_PREFIX)) return null;
  const m = /^http:\/\/127\.0\.0\.1:(\d{1,5})\/?$/.exec(line.slice(READY_PREFIX.length).trim());
  if (!m) return null;
  const port = Number(m[1]);
  if (port < 1 || port > 65535) return null;
  return { url: `http://127.0.0.1:${port}`, port };
}

/** extractErrorReason: the text of the last "ERROR: ..." line on stderr, or null. */
function extractErrorReason(lines) {
  for (let i = lines.length - 1; i >= 0; i--) {
    const m = /^\s*ERROR:\s*(.*\S)\s*$/.exec(lines[i]);
    if (m) return m[1].length > MAX_REASON_CHARS ? `${m[1].slice(0, MAX_REASON_CHARS)}…` : m[1];
  }
  return null;
}

/**
 * isPortInUseError: the service could not listen on the port we picked
 * (taken between our check and its bind, or inside a Windows excluded range).
 */
function isPortInUseError(reason) {
  if (typeof reason !== 'string') return false;
  return /address already in use|only one usage of each socket address|EADDRINUSE|WSAEADDRINUSE|forbidden by its access permissions|bind: permission denied/i.test(reason);
}

/** redact replaces every occurrence of secret in text. */
function redact(text, secret) {
  if (!secret || typeof text !== 'string') return text;
  return text.split(secret).join('[token]');
}

/**
 * resolveServerCommand: where the service executable is.
 *   packaged:   {resources}/bin/lectureflow(.exe), resources dir = process.resourcesPath
 *   development: LECTUREFLOW_SERVER (default: {repo}/lectureflow(.exe)),
 *               LECTUREFLOW_RESOURCES (default: the repository root, worker in worker/).
 *               A LECTUREFLOW_SERVER ending in .js/.cjs/.mjs is run with the
 *               Electron binary as Node (tests use a fake service this way).
 * Returns { command, prefixArgs, resourcesDir, extraEnv }.
 */
function resolveServerCommand({ isPackaged, resourcesPath, env = {}, repoRoot, platform, execPath, pathMod = path }) {
  const exe = platform === 'win32' ? 'lectureflow.exe' : 'lectureflow';
  if (isPackaged) {
    return { command: pathMod.join(resourcesPath, 'bin', exe), prefixArgs: [], resourcesDir: resourcesPath, extraEnv: {} };
  }
  const resourcesDir = env.LECTUREFLOW_RESOURCES ? pathMod.resolve(env.LECTUREFLOW_RESOURCES) : repoRoot;
  const command = env.LECTUREFLOW_SERVER ? pathMod.resolve(env.LECTUREFLOW_SERVER) : pathMod.join(repoRoot, exe);
  if (/\.(c|m)?js$/i.test(command)) {
    return { command: execPath, prefixArgs: [command], resourcesDir, extraEnv: { ELECTRON_RUN_AS_NODE: '1' } };
  }
  return { command, prefixArgs: [], resourcesDir, extraEnv: {} };
}

module.exports = {
  READY_PREFIX,
  EXIT_RESTART,
  generateToken,
  buildServerArgs,
  parseReadyLine,
  extractErrorReason,
  isPortInUseError,
  redact,
  resolveServerCommand,
};
