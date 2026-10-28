// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Talks to the native host the way the browser does (4-byte little-endian
// length + JSON on its stdin / stdout), for the protocol answers the
// extension never provokes on its own: an enqueue of a session that is
// already imported, and of a DRM-protected track.

import { spawn } from 'node:child_process';

/** Runs the host (the test's .bat), sends the messages in order, returns the replies (and pushes). */
export async function exchange(bat, messages, { timeout = 30000 } = {}) {
  // the way Chrome starts a host on Windows: cmd /d /s /c ""host" origin"
  const child = spawn('cmd.exe', ['/d', '/s', '/c', `""${bat}" chrome-extension://lf-native-e2e-direct/"`], { stdio: ['pipe', 'pipe', 'ignore'], windowsHide: true, windowsVerbatimArguments: true });
  const frames = [];
  let buf = Buffer.alloc(0);
  const waiters = [];
  let exited = null;
  child.on('close', (code) => {
    exited = code;
    for (const w of waiters.splice(0)) w();
  });
  child.stdout.on('data', (chunk) => {
    buf = Buffer.concat([buf, chunk]);
    while (buf.length >= 4) {
      const n = buf.readUInt32LE(0);
      if (buf.length < 4 + n) break;
      frames.push(JSON.parse(buf.subarray(4, 4 + n).toString('utf8')));
      buf = buf.subarray(4 + n);
      for (const w of waiters.splice(0)) w();
    }
  });
  const write = (m) => {
    const body = Buffer.from(JSON.stringify(m), 'utf8');
    const head = Buffer.alloc(4);
    head.writeUInt32LE(body.length, 0);
    child.stdin.write(Buffer.concat([head, body]));
  };
  const replyTo = async (id) => {
    const end = Date.now() + timeout;
    for (;;) {
      const f = frames.find((x) => x.id === id);
      if (f) return f;
      if (exited !== null) throw new Error(`the host exited (code ${exited}) without answering ${id}`);
      if (Date.now() > end) throw new Error(`no reply to ${id}`);
      await new Promise((r) => {
        waiters.push(r);
        setTimeout(r, 200);
      });
    }
  };
  const replies = [];
  try {
    for (const [i, m] of messages.entries()) {
      const id = `lf-native-${i + 1}`;
      write({ ...m, id });
      replies.push(await replyTo(id));
    }
  } finally {
    child.stdin.end();
    await new Promise((r) => {
      if (exited !== null) return r();
      child.on('close', r);
      setTimeout(() => {
        child.kill();
        r();
      }, 10000);
    });
  }
  return { replies, pushes: frames.filter((f) => f.type === 'status') };
}

export const HELLO = { type: 'hello', protocol: 1, extension: { version: '0.0.0-native-e2e', browser: 'chrome', locale: 'en' } };
