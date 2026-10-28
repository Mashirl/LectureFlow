// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Runs the isolated LectureFlow of the native end-to-end test in desktop
// mode, as the desktop shell would: the access token on the first line of
// stdin, stdin kept open (its end stops the app gracefully), stdout and
// stderr to log files. The test starts it, and so does the native host
// through launch.json (LectureFlow is not running → the host starts it), so
// it talks to the test through files in its state directory only:
//
//   node supervisor.mjs <state dir> <who started it>
//
//   instance.json  in:  { exe, args, env, unset, token, logDir }
//   launches.log   out: one line per start, with who started it
//   url.txt        out: http://127.0.0.1:PORT once ready (removed on exit)
//   app.pid        out: the app's process id
//   stop.flag      in:  present → close stdin, wait for the exit
//   exit.txt       out: how the app ended

import fs from 'node:fs';
import path from 'node:path';
import { spawn } from 'node:child_process';

const [stateDir, who = 'test'] = process.argv.slice(2);
const at = (name) => path.join(stateDir, name);
const cfg = JSON.parse(fs.readFileSync(at('instance.json'), 'utf8'));
const stamp = () => new Date().toISOString();

const env = {};
const drop = new Set(cfg.unset.map((k) => k.toUpperCase()));
for (const [k, v] of Object.entries(process.env)) if (!drop.has(k.toUpperCase())) env[k] = v;
for (const [k, v] of Object.entries(cfg.env)) {
  for (const old of Object.keys(env)) if (old.toUpperCase() === k.toUpperCase()) delete env[old];
  env[k] = v;
}

fs.mkdirSync(cfg.logDir, { recursive: true });
const n = fs.existsSync(at('launches.log')) ? fs.readFileSync(at('launches.log'), 'utf8').split('\n').filter((l) => / start \d+ /.test(l)).length + 1 : 1;
fs.appendFileSync(at('launches.log'), `${stamp()} start ${n} by ${who} supervisor=${process.pid}\n`);
fs.rmSync(at('url.txt'), { force: true });
fs.rmSync(at('exit.txt'), { force: true });
fs.rmSync(at('stop.flag'), { force: true });

const out = fs.openSync(path.join(cfg.logDir, `app-${n}.stdout.log`), 'a');
const err = fs.openSync(path.join(cfg.logDir, `app-${n}.stderr.log`), 'a');
const child = spawn(cfg.exe, cfg.args, { env, stdio: ['pipe', 'pipe', err], windowsHide: true });
fs.writeFileSync(at('app.pid'), String(child.pid));
child.stdin.write(`${cfg.token}\n`);

let buf = '';
child.stdout.on('data', (chunk) => {
  fs.writeSync(out, chunk);
  buf += chunk.toString('utf8');
  let i;
  while ((i = buf.indexOf('\n')) >= 0) {
    const line = buf.slice(0, i).replace(/\r$/, '');
    buf = buf.slice(i + 1);
    if (line.startsWith('LECTUREFLOW_READY ')) {
      fs.writeFileSync(at('url.txt.tmp'), line.slice('LECTUREFLOW_READY '.length).trim());
      fs.renameSync(at('url.txt.tmp'), at('url.txt'));
    }
  }
});

let stopping = false;
const timer = setInterval(() => {
  if (stopping || !fs.existsSync(at('stop.flag'))) return;
  stopping = true;
  fs.rmSync(at('stop.flag'), { force: true });
  child.stdin.end();
  setTimeout(() => child.kill(), 40000).unref();
}, 200);

child.on('exit', (code, signal) => {
  clearInterval(timer);
  fs.rmSync(at('url.txt'), { force: true });
  fs.writeFileSync(at('exit.txt'), `${stamp()} code=${code} signal=${signal || ''} requested=${stopping}\n`);
  fs.appendFileSync(at('launches.log'), `${stamp()} exit ${n} code=${code} requested=${stopping}\n`);
  fs.closeSync(out);
  fs.closeSync(err);
  process.exit(0);
});
