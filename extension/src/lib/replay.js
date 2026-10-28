// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Replaying a redacted report (src/lib/report.js): generic detection runs
// again on the report's inputs, with its fetches answered from the report,
// and the result is compared with what the report says the browser found.
// Used by the fixture tests now and by the profile CI (M3). Pure.

import { detectTracks, summarize } from './detect.js';
import { redactTrack } from './report.js';
import { applyProfile } from './profile.js';
import { combineObserved } from './observe.js';

/** Runs detection on a report's inputs. */
export async function replayReport(report) {
  const byUrl = new Map();
  for (const f of report.inputs.fetches || []) {
    const key = `${f.purpose === 'file' ? 'file' : 'text'}\u0000${f.url}`;
    if (!byUrl.has(key)) byUrl.set(key, f);
  }
  const io = {
    async fetchText(url) {
      const f = byUrl.get(`text\u0000${url}`);
      if (!f) return { ok: false, status: 0, contentType: '', text: '', error: 'not in report' };
      if (f.truncated) return { ok: false, status: f.status, contentType: f.contentType, text: '', error: 'truncated' };
      return { ok: f.ok, status: f.status, contentType: f.contentType, text: f.text || '', error: f.error };
    },
    async probeFile(url) {
      const f = byUrl.get(`file\u0000${url}`);
      if (!f) return { ok: false, status: 0, contentType: '', size: -1, error: 'not in report' };
      return { ok: f.ok, status: f.status, contentType: f.contentType, size: f.size ?? -1, error: f.error };
    },
  };
  return detectTracks({ frames: report.inputs.frames, webRequests: report.inputs.webRequests }, io);
}

/**
 * Differences between a replay and the report's expected result; empty
 * when the replay reproduces it.
 */
export function compareReplay(report, detection) {
  const diffs = [];
  // The replay ran on redacted URLs already: map them through unchanged
  // (only re-parsed, like the expected side), but shape names the same way.
  const same = { url: normUrl, segment: (x) => x };
  const got = detection.tracks.map((t) => normalizeForCompare(redactTrack(same, t)));
  const want = report.expected.tracks.map(normalizeForCompare);
  const gotSummary = summarize(detection.tracks);
  for (const k of Object.keys(report.expected.summary)) {
    if (gotSummary[k] !== report.expected.summary[k]) diffs.push(`summary.${k}: expected ${report.expected.summary[k]}, got ${gotSummary[k]}`);
  }
  const max = Math.max(got.length, want.length);
  for (let i = 0; i < max; i++) {
    const a = JSON.stringify(want[i]);
    const b = JSON.stringify(got[i]);
    if (a !== b) diffs.push(`tracks[${i}]: expected ${a}, got ${b}`);
  }
  const wantRuns = (report.expected.segmentRuns || []).map((s) => s.count).join(',');
  const gotRuns = (detection.segmentRuns || []).map((s) => s.count).join(',');
  if (wantRuns !== gotRuns) diffs.push(`segmentRuns: expected [${wantRuns}], got [${gotRuns}]`);
  return diffs;
}

function normUrl(u) {
  try {
    const x = new URL(u);
    x.hash = '';
    return x.href;
  } catch {
    return String(u);
  }
}

function normId(id) {
  const i = String(id).indexOf('#lf=');
  return i < 0 ? normUrl(id) : normUrl(id.slice(0, i)) + id.slice(i);
}

// Fields compared: everything a track says; URLs re-parsed on both sides.
function normalizeForCompare(t) {
  if (!t) return t;
  const out = { ...t, id: normId(t.id), url: normUrl(t.url) };
  if (out.variants) out.variants = out.variants.map((v) => (v.url ? { ...v, url: normUrl(v.url) } : v));
  return Object.fromEntries(Object.entries(out).sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0)));
}

/**
 * Applies a profile to a replay and compares the naming with the report:
 * the same tracks get the same roles, numbering and recommendation.
 */
export function compareProfile(report, profile, detection, locale = 'en') {
  const diffs = [];
  const exp = report.expected.profile;
  if (!exp) return ['report has no profile result'];
  if (exp.id !== profile.id) diffs.push(`profile id: report ${exp.id}, file ${profile.id}`);
  // Response values the report recorded (observe), under the profile's
  // version they were taken with.
  const frames = (report.inputs.frames || []).map((f) => ({
    frameId: f.frameId,
    observed: (f.observed || []).map((o) => ({ ...o, key: `${profile.id}:${profile.version}` })),
  }));
  const named = applyProfile(profile, detection.tracks, locale, combineObserved(profile, frames));
  const got = named.tracks.map((t) => `${normId(t.id)}|${t.role}|${t.labelIndex || 0}|${!!t.recommended}`);
  const want = exp.tracks.map((t) => `${normId(t.id)}|${t.role}|${t.labelIndex || 0}|${!!t.recommended}`);
  if (got.length !== want.length) diffs.push(`named tracks: expected ${want.length}, got ${got.length}`);
  for (let i = 0; i < Math.max(got.length, want.length); i++) {
    if (got[i] !== want[i]) diffs.push(`named[${i}]: expected ${want[i]}, got ${got[i]}`);
  }
  if (named.others.length !== exp.others) diffs.push(`others: expected ${exp.others}, got ${named.others.length}`);
  return diffs;
}
