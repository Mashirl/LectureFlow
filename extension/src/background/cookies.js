// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The only place the extension reads cookies (SPEC-ext 隐私: Cookie 只在用户点
// “送进 LectureFlow”时读取，只读轨道所在域名). src/background/actions.js calls
// it when the user clicks "送进 LectureFlow" or "续传", with the URLs of the
// tracks the user ticked (src/lib/send.js cookieTargets) and the tab they
// were found in (src/lib/partition.js cookieScope: the partitioned cookies
// of an embedded player). The cookies go straight into that one message to
// LectureFlow; nothing here keeps, logs or stores them.

import { browser } from './env.js';
import { readCookiesWith } from '../lib/partition.js';

// One indirection so the end-to-end test can record the getAll calls
// (their arguments, never the answers); the e2e build only.
// getPartitionKey: Chrome 132+ only, looked up at each call. Firefox has
// none (web-ext lint warns UNSUPPORTED_API about this one line; known).
export const cookieApi = {
  getAll: (details) => browser.cookies.getAll(details),
  get getPartitionKey() {
    const fn = browser.cookies.getPartitionKey; // bound to chrome.cookies by the polyfill
    return typeof fn === 'function' ? fn : undefined;
  },
};

/**
 * The cookies the browser would send to each URL from that tab, each
 * cookie once. scope: { tabId, frameIds, topUrl } (src/lib/partition.js).
 */
export async function readCookies(urls, scope = {}) {
  return readCookiesWith(cookieApi, urls, scope);
}
