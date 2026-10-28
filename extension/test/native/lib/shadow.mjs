// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The store build puts its in-page buttons and cards in closed shadow roots
// (src/content/inpage.js), which Playwright's selectors do not enter. The
// DevTools protocol does: DOM.getDocument with pierce lists closed shadow
// roots too. These helpers find an element inside the shadow root of a host
// (lectureflow-ui[data-kind=…]), read it, and click it with a real mouse
// click at its place on the screen. A frame of another site (the platform
// inside an LMS page) has its own target: its session comes from
// context.newCDPSession(frame), and its coordinates are shifted by the
// iframe's place in the page.

const sessions = new WeakMap();
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// A frame must be of another site (an out-of-process iframe with its own
// target): its coordinates are then its own, shifted by the iframe's box.
async function sessionFor(context, target) {
  if (!sessions.has(target)) sessions.set(target, await context.newCDPSession(target));
  return sessions.get(target);
}

/** nodeId of the first innerSel inside the shadow root of a hostSel host (0 = none). */
async function find(cdp, hostSel, innerSel) {
  const { root } = await cdp.send('DOM.getDocument', { depth: -1, pierce: true });
  const byId = new Map();
  const walk = (n) => {
    byId.set(n.nodeId, n);
    for (const c of n.children || []) walk(c);
    for (const s of n.shadowRoots || []) walk(s);
    if (n.contentDocument) walk(n.contentDocument);
  };
  walk(root);
  const { nodeIds } = await cdp.send('DOM.querySelectorAll', { nodeId: root.nodeId, selector: hostSel });
  for (const hostId of nodeIds) {
    const host = byId.get(hostId);
    const sr = host && host.shadowRoots && host.shadowRoots[0];
    if (!sr) continue;
    if (!innerSel) return sr.nodeId;
    const { nodeId } = await cdp.send('DOM.querySelector', { nodeId: sr.nodeId, selector: innerSel });
    if (nodeId) return nodeId;
  }
  return 0;
}

async function call(cdp, nodeId, fn) {
  const { object } = await cdp.send('DOM.resolveNode', { nodeId });
  const r = await cdp.send('Runtime.callFunctionOn', { objectId: object.objectId, functionDeclaration: fn.toString(), returnByValue: true });
  await cdp.send('Runtime.releaseObject', { objectId: object.objectId }).catch(() => {});
  if (r.exceptionDetails) throw new Error(r.exceptionDetails.text);
  return r.result.value;
}

const describe = function () {
  const ds = {};
  for (const k of Object.keys(this.dataset || {})) ds[k] = this.dataset[k];
  return { text: (this.textContent || '').replace(/\s+/g, ' ').trim(), dataset: ds, disabled: !!this.disabled, checked: !!this.checked, pressed: this.getAttribute && this.getAttribute('aria-pressed') };
};

/**
 * In-page UI of a page or frame. scope: a Playwright Page or Frame; page:
 * the Page (for the mouse).
 */
export function shadowUI(context, page, scope = page) {
  const cdpP = () => sessionFor(context, scope);
  const api = {
    /** { text, dataset, disabled, checked, pressed } of the element, or null. */
    async read(hostSel, innerSel) {
      const cdp = await cdpP();
      const id = await find(cdp, hostSel, innerSel);
      return id ? call(cdp, id, describe) : null;
    },
    /** Every match's description (inside every host). */
    async readAll(hostSel, innerSel) {
      const cdp = await cdpP();
      const { root } = await cdp.send('DOM.getDocument', { depth: -1, pierce: true });
      const byId = new Map();
      const walk = (n) => {
        byId.set(n.nodeId, n);
        for (const c of n.children || []) walk(c);
        for (const s of n.shadowRoots || []) walk(s);
      };
      walk(root);
      const out = [];
      const { nodeIds } = await cdp.send('DOM.querySelectorAll', { nodeId: root.nodeId, selector: hostSel });
      for (const hostId of nodeIds) {
        const sr = byId.get(hostId)?.shadowRoots?.[0];
        if (!sr) continue;
        const { nodeIds: inner } = await cdp.send('DOM.querySelectorAll', { nodeId: sr.nodeId, selector: innerSel });
        for (const id of inner) out.push(await call(cdp, id, describe));
      }
      return out;
    },
    /** Waits until the element exists (and pred(description) holds). */
    async waitFor(hostSel, innerSel, { timeout = 15000, pred = () => true } = {}) {
      const end = Date.now() + timeout;
      let last = null;
      while (Date.now() < end) {
        try {
          last = await api.read(hostSel, innerSel);
          if (last && pred(last)) return last;
        } catch {
          // the document changed under us: try again
        }
        await sleep(150);
      }
      throw new Error(`timed out waiting for ${hostSel} >>> ${innerSel}${last ? ` (last: ${JSON.stringify(last).slice(0, 300)})` : ''}`);
    },
    /** A real mouse click in the middle of the element. */
    async click(hostSel, innerSel, { timeout = 15000 } = {}) {
      await api.waitFor(hostSel, innerSel, { timeout });
      const cdp = await cdpP();
      const id = await find(cdp, hostSel, innerSel);
      if (!id) throw new Error(`gone: ${hostSel} >>> ${innerSel}`);
      if (scope !== page) await (await scope.frameElement()).scrollIntoViewIfNeeded();
      await cdp.send('DOM.scrollIntoViewIfNeeded', { nodeId: id });
      const { quads } = await cdp.send('DOM.getContentQuads', { nodeId: id });
      if (!quads || !quads.length) throw new Error(`not visible: ${hostSel} >>> ${innerSel}`);
      const q = quads[0];
      let x = (q[0] + q[2] + q[4] + q[6]) / 4;
      let y = (q[1] + q[3] + q[5] + q[7]) / 4;
      if (scope !== page) {
        const box = await (await scope.frameElement()).boundingBox();
        x += box.x;
        y += box.y;
      }
      await page.mouse.click(x, y);
    },
  };
  return api;
}
