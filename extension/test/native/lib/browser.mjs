// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Playwright's own Chrome for Testing (tools/ms-playwright; a throw-away
// profile in the work directory; never the user's browser) with a copy of
// the store build, dist/chrome. The copy differs in one place: the test
// sites are listed in host_permissions, i.e. granted at install — what the
// user does with "在此网站自动探测" and the browser's permission prompt,
// which a test cannot click. The code, the closed shadow roots, the
// missing test hooks and the extension id (the manifest's key) are the
// store build's. The LMS site is deliberately not granted.
//
// The test host names resolve to 127.0.0.1, everything else fails to
// resolve: the browser reaches nothing but the local servers.

import fs from 'node:fs';
import path from 'node:path';

export const GRANTED = ['https://*.cloud.panopto.eu/*', 'http://127.0.0.1/*'];

function copyDir(from, to) {
  fs.mkdirSync(to, { recursive: true });
  for (const e of fs.readdirSync(from, { withFileTypes: true })) {
    const s = path.join(from, e.name);
    const d = path.join(to, e.name);
    if (e.isDirectory()) copyDir(s, d);
    else fs.copyFileSync(s, d);
  }
}

/** Copies dist/chrome into dir with the test sites granted; returns dir. */
export function prepareExtension(distChrome, dir) {
  fs.rmSync(dir, { recursive: true, force: true });
  copyDir(distChrome, dir);
  const file = path.join(dir, 'manifest.json');
  const m = JSON.parse(fs.readFileSync(file, 'utf8'));
  if (!m.key) throw new Error('dist/chrome has no key: not the development build');
  m.host_permissions = GRANTED;
  fs.writeFileSync(file, JSON.stringify(m, null, 2) + '\n');
  return dir;
}

export async function launchBrowser({ extDir, userDataDir, repo }) {
  if (!process.env.PLAYWRIGHT_BROWSERS_PATH) process.env.PLAYWRIGHT_BROWSERS_PATH = path.join(repo, 'tools', 'ms-playwright');
  const { chromium } = await import('playwright');
  fs.rmSync(userDataDir, { recursive: true, force: true });
  const context = await chromium.launchPersistentContext(userDataDir, {
    channel: 'chromium',
    headless: true,
    viewport: { width: 1280, height: 860 },
    ignoreHTTPSErrors: true,
    args: [
      `--disable-extensions-except=${extDir}`,
      `--load-extension=${extDir}`,
      '--lang=zh-CN',
      '--ignore-certificate-errors',
      '--host-resolver-rules=MAP *.lf-e2e.test 127.0.0.1, MAP *.cloud.panopto.eu 127.0.0.1, MAP * ~NOTFOUND, EXCLUDE 127.0.0.1',
    ],
  });
  const sw = context.serviceWorkers()[0] || (await context.waitForEvent('serviceworker', { timeout: 20000 }));
  const extensionId = new URL(sw.url()).host;
  /** The live service worker (the browser may have restarted it). */
  const worker = async () => {
    const live = context.serviceWorkers().find((w) => w.url().includes(extensionId));
    if (live) return live;
    return context.waitForEvent('serviceworker', { timeout: 20000 });
  };
  return { context, extensionId, worker };
}

/** Waits until the background registered the in-page and page-observer scripts for the granted platform. */
export async function waitContentScripts(worker, timeout = 20000) {
  const end = Date.now() + timeout;
  let ids = [];
  while (Date.now() < end) {
    ids = await (await worker()).evaluate(async () => (await chrome.scripting.getRegisteredContentScripts()).map((s) => `${s.id}:${s.world || 'ISOLATED'}:${s.matches.join(' ')}`));
    if (ids.some((s) => s.startsWith('lf-inpage:') && s.includes('cloud.panopto.eu')) && ids.some((s) => s.startsWith('lf-observe-main:MAIN:') && s.includes('cloud.panopto.eu'))) return ids;
    await new Promise((r) => setTimeout(r, 200));
  }
  throw new Error(`content scripts not registered: ${ids.join(', ')}`);
}

export async function tabIdOf(worker, page) {
  const url = page.url();
  return (await worker()).evaluate(async (u) => {
    const t = (await chrome.tabs.query({})).find((x) => x.url === u);
    return t ? t.id : null;
  }, url);
}
