// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
//
// colab-obs-web: the logic of the Colab area run in Node (web/colab_obs_test.go
// starts it with the web directory as a file: URL). settings_colab.js is
// imported as the page imports it, with the few browser globals its
// imports read at load stubbed; t() answers the keys (no catalog loaded).
// Checks: the memory bar's segments (order, shares, the free part, the
// used / free form), the accelerators an account unlocks and why the others
// are locked, the lectures the balance covers (the free quota on the T4),
// the collapsed rail's Colab cell, the speed-mode parts of a lecture and the
// card the area reads from remotes[] and the status.

import assert from 'node:assert/strict';

const store = { getItem: () => null, setItem() {}, removeItem() {} };
const el = () => ({ dataset: {}, style: {}, classList: { add() {}, remove() {}, toggle() {} }, setAttribute() {}, append() {}, addEventListener() {} });
Object.assign(globalThis, {
  location: { search: '', hash: '', pathname: '/', origin: 'http://127.0.0.1' },
  localStorage: store,
  sessionStorage: store,
  document: { documentElement: el(), body: el(), addEventListener() {}, querySelector: () => null, getElementById: () => null, createElement: el },
  addEventListener() {},
  matchMedia: () => ({ matches: false, addEventListener() {} }),
});
globalThis.window = globalThis;

const C = await import(new URL('settings_colab.js', process.argv[2]).href);

// ---- the memory bar ----
{
  const u = { ram: [{ kind: 'free', mb: 43244 }, { kind: 'data', mb: 491 }, { kind: 'ramdisk', mb: 3441 }, { kind: 'system', mb: 2365 }, { kind: 'whisper', mb: 4731 }, { kind: 'gpu', mb: 99 }] };
  const lay = C.ramSegments(u);
  assert.deepEqual(lay.segs.map((s) => s.kind), ['ramdisk', 'whisper', 'system', 'data'], 'segments left to right whatever the order sent; unknown kinds left out');
  assert.equal(lay.total, 54272, 'the total is the segments with the free part');
  assert.equal(lay.free, 43244);
  const sum = lay.segs.reduce((n, s) => n + s.frac, 0) + lay.freeFrac;
  assert.ok(Math.abs(sum - 1) < 1e-9, 'the shares and the free part make the whole bar: ' + sum);
  for (const s of lay.segs) assert.ok(Math.abs(s.frac - s.mb / 54272) < 1e-12, s.kind + ' share');
  // the data segment is the narrowest: under 3 px of a 288 px bar, so the CSS minimum decides its width
  const data = lay.segs.find((s) => s.kind === 'data');
  assert.ok(data.frac * 288 < 3, 'the data segment needs the 3 px minimum: ' + data.frac * 288);
  // segments of 0 MB are left out; negative readings count as 0
  const disk = C.ramSegments({ ram: [{ kind: 'ramdisk', mb: 0 }, { kind: 'whisper', mb: 3390 }, { kind: 'data', mb: -5 }, { kind: 'free', mb: 610 }] });
  assert.deepEqual(disk.segs.map((s) => s.kind), ['whisper']);
  assert.equal(disk.total, 4000);
  // the runtime's own report: used and free only
  const b = C.ramSegments({ ram: [{ kind: 'used', mb: 4100 }, { kind: 'free', mb: 50172 }] });
  assert.deepEqual(b.segs.map((s) => s.kind), ['used']);
  assert.ok(Math.abs(b.segs[0].frac + b.freeFrac - 1) < 1e-9);
  assert.equal(C.ramSegments({ ram: [] }), null);
  assert.equal(C.ramSegments(null), null);
}

// ---- the accelerators an account unlocks ----
const paid = { read_at: '2026-10-27T06:00:00Z', read_ok: true, level: 'paid', paid_balance_cu: 286.4, free_quota_cu: null, eligible_gpus: ['G4', 'A100', 'L4', 'T4'], ineligible_gpus: ['H100'], eligibility_known: true };
const unknown = { read_at: null, read_ok: false, read_error: { code: 'colab.unavailable' }, level: 'free', paid_balance_cu: null, eligible_gpus: [], ineligible_gpus: [], eligibility_known: false };
const free = { read_at: '2026-10-27T06:00:00Z', read_ok: true, level: 'free', paid_balance_cu: 0, free_quota_cu: 3.2, free_refill_at: '2026-10-27T15:00:00Z', eligible_gpus: ['T4'], ineligible_gpus: ['L4'], eligibility_known: true };
const drained = { ...paid, level: 'free', paid_balance_cu: 0 };
{
  const st = (account, selectable) => ({ account, selectable_accelerators: selectable });
  // the account was not read: only the T4, every other GPU locked with that reason
  const u = st(unknown, ['T4']);
  assert.equal(C.lockOf(u, 'T4'), '');
  for (const x of ['L4', 'A100', 'G4', 'H100']) assert.equal(C.lockOf(u, x), 'account_unknown', x);
  assert.equal(C.accountKnown(unknown), false);
  assert.equal(C.accountNeedsCheck(unknown), true);
  assert.deepEqual(C.accountError(unknown), { code: 'colab.unavailable' });
  // no paid balance (the free quota is for the T4 only): no_balance
  assert.equal(C.lockOf(st(free, ['T4']), 'T4'), '', 'the T4 is not locked with a free quota');
  assert.equal(C.lockOf(st(free, ['T4']), 'L4'), 'no_balance');
  assert.equal(C.lockOf(st(drained, ['T4']), 'A100'), 'no_balance');
  // a paid account: what Colab does not offer it
  const p = st(paid, ['T4', 'L4', 'A100', 'G4']);
  assert.equal(C.lockOf(p, 'L4'), '');
  assert.equal(C.lockOf(p, 'H100'), 'ineligible');
  assert.equal(C.accountNeedsCheck(paid), false);
  assert.equal(C.accountError(paid), null);
  // a back end that does not list them: nothing locked
  assert.equal(C.lockOf({ account: paid }, 'H100'), '');
  assert.equal(C.effectiveAccel({ accelerator: 'L4', effective_accelerator: 'T4' }), 'T4');
  assert.equal(C.effectiveAccel({ accelerator: 'L4' }), 'L4');
}

// ---- lectures the balance covers ----
{
  assert.equal(C.fitFor(paid, 'T4'), 4200, 'T4: 286.40 / (3.8 min x 1.07 CU/h)');
  assert.equal(C.fitFor(paid, 'L4'), 3700);
  assert.equal(C.fitFor(paid, 'A100'), 1400);
  assert.equal(C.fitFor(paid, 'G4'), 1000);
  assert.equal(C.fitFor(paid, 'H100'), null, 'never measured');
  assert.equal(C.fitFor(free, 'T4'), 47, 'the free quota counts on the T4');
  assert.equal(C.fitFor(free, 'L4'), 0, 'but not on the other GPUs');
  assert.equal(C.fitFor({ ...paid, free_quota_cu: 3.2 }, 'T4'), 4300, 'paid balance and free quota together on the T4');
  assert.equal(C.fitFor(unknown, 'T4'), null, 'no balance read');
  assert.equal(C.approxCount(99.7), 99);
  assert.equal(C.approxCount(104), 100);
  assert.equal(C.approxCount(4224), 4200);
}

// ---- the collapsed rail's Colab cell ----
{
  const active = {
    st: { state: 'running', accelerator: 'L4', effective_accelerator: 'L4', account: paid },
    machine: { gpu_name: 'NVIDIA L4', requested: 'L4', vram_total_mb: 23034 },
    usage: { gpu_util_pct: 91, ram: [{ kind: 'ramdisk', mb: 3441 }, { kind: 'whisper', mb: 4731 }, { kind: 'system', mb: 2365 }, { kind: 'data', mb: 491 }, { kind: 'free', mb: 43244 }] },
    jobs: [],
  };
  const a = C.railColab(active);
  assert.match(a.value, /91/);
  assert.equal(a.sub, 'L4');
  assert.equal(a.warn, false);
  assert.equal(a.mini.segs.length, 4, 'the mini memory bar has the four segments');
  const idle = C.railColab({ st: { state: 'none', accelerator: 'L4', account: paid }, jobs: [] });
  assert.equal(idle.value, '286', 'the compute units left, whole');
  assert.equal(idle.sub, 'CU');
  assert.equal(idle.mini, null);
  const q = C.railColab({ st: { state: 'none', accelerator: 'T4', account: free }, jobs: [] });
  assert.equal(q.value, '3', 'no paid balance: the free quota');
  assert.equal(q.warn, false);
  const u = C.railColab({ st: { state: 'none', accelerator: 'T4', account: unknown }, jobs: [] });
  assert.equal(u.value, '--');
  assert.equal(u.warn, true, 'not read: the warning colour');
  assert.equal(u.sub, 'resources.remote.cards_t4');
  assert.equal(u.title, 'resources.remote.unknown_t4');
  assert.equal(C.railColab(null), null, 'Colab off: no cell');
}

// ---- speed mode on the lecture page ----
{
  const lec = (split) => ({ stages: [{ stage: 'prep' }, { stage: 'asr', asr_timing: split === undefined ? { load_ms: 1 } : { postprocess: { split } } }] });
  assert.equal(C.splitParts(lec({ parts: 4, cuts_ms: [1, 2, 3] })), 4);
  assert.equal(C.splitParts(lec({ parts: 1, cuts_ms: [] })), 0, 'one part is the whole lecture');
  assert.equal(C.splitParts(lec(undefined)), 0);
  assert.equal(C.splitParts({ stages: [] }), 0);
}

// ---- the card: remotes[] with the status ----
{
  const status = { state: 'running', accelerator: 'L4', planned: 3, uploaded_ahead: 1, jobs: [{ step: 'remote_transcribe', stage: 'transcribe', overall: 0.5 }], machine: { gpu_name: 'old' } };
  const card = { id: 'colab:remote', kind: 'colab', machine: { gpu_name: 'NVIDIA L4' }, usage: { ram: [] }, queued: 3, uploaded_ahead: 1, jobs: [{ lecture_id: 'a', lecture_title: 'L7', stage: 'transcribe', progress: 0.62, eta_sec: 70, split: 0 }, { lecture_id: 'b', lecture_title: 'L8', stage: 'upload', progress: 0.1 }] };
  const c = C.colabCard({ remote: status, remotes: [{ kind: 'ssh' }, card] });
  assert.equal(c.st, status);
  assert.equal(c.machine.gpu_name, 'NVIDIA L4', 'the card first');
  assert.deepEqual(c.jobs.map((j) => j.title), ['L7', 'L8']);
  assert.equal(c.queued, 3);
  assert.equal(c.ahead, 1);
  const s = C.colabCard({ remote: status });
  assert.equal(s.machine.gpu_name, 'old', 'without remotes[]: the status');
  assert.equal(s.jobs.length, 1);
  assert.equal(s.jobs[0].step, 'remote_transcribe');
  assert.equal(s.queued, 3);
  assert.equal(C.colabCard({ remotes: [] }), null, 'Colab off');
  assert.equal(C.colabCard({ remotes: [{ ...card, colab: status }] }).st, status, "the card's own colab part");
}

// ---- the GPU the remote reports against the one asked for ----
{
  assert.equal(C.gpuMismatch({ gpu_name: 'Tesla T4', requested: 'T4' }), false);
  assert.equal(C.gpuMismatch({ gpu_name: 'NVIDIA RTX PRO 6000 Blackwell Server Edition', requested: 'G4' }), false);
  assert.equal(C.gpuMismatch({ gpu_name: 'Tesla T4', requested: 'L4' }), true);
  assert.equal(C.gpuMismatch({ gpu_name: 'NVIDIA L4' }), false);
}

console.log('colab-obs checks: ok');
