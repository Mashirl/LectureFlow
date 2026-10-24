// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// Pure helpers shared by the views: no DOM, no network, no timers.
// Everything here is deterministic so it stays easy to reason about
// (SPEC §9.5 waiting_for texts, §9.6 word diff, formatting). UI text comes
// from t() (SPEC-v2 P14.2); the coded fields of the back end (P14.3: stale
// reasons, errors, notes, warnings) are rendered here, so every view shows
// them the same way.

import { t, has, getLocale, PSEUDO } from './i18n.js';

export const LECTURE_STAGES = ['prep', 'asr', 'summary', 'repair', 'translate', 'export'];

// t(prefix + code) when the catalog has it, else the code itself (an id the
// front end does not know yet stays readable instead of showing ⟦key⟧).
function named(prefix, code) {
  return code && has(prefix + code) ? t(prefix + code) : code;
}

// A read-only name table whose values are looked up with t() on every read,
// so a language switch needs no reload. Unknown keys read as undefined
// (callers fall back to the raw code); Object.keys/entries work.
function nameTable(prefix, keys) {
  const known = new Set(keys);
  const value = (k) => (typeof k === 'string' && known.has(k) ? t(prefix + k) : undefined);
  return new Proxy(
    {},
    {
      get: (_, k) => value(k),
      has: (_, k) => known.has(k),
      ownKeys: () => [...keys],
      getOwnPropertyDescriptor: (_, k) => (known.has(k) ? { value: value(k), enumerable: true, configurable: true, writable: false } : undefined),
    },
  );
}

/** List of items with the current language's list separator. */
export function listJoin(items) {
  return (items || []).join(t('common.list_sep'));
}

/** Clauses joined with the current language's clause separator (compound notes, several stale reasons). */
export function clauseJoin(items) {
  return (items || []).join(t('common.clause_sep'));
}

// Stage names: stage.{id} (lecture, course and material stages; v2 G3.1
// glossary_review; v2 auxiliary tasks).
export function stageName(stage) {
  return named('stage.', stage);
}

// Role names: role.{id} (the LLM roles of the settings page).
export function roleName(role) {
  return named('role.', role);
}

// Thinking levels of a role (model.ThinkingLevels; "off" is the default).
export const THINKING_LEVELS = ['off', 'low', 'high', 'max'];

// The label of a model snapshot (StageModel, SnapshotView) in the interface
// language: "DeepSeek / deepseek-flash · temperature 0.3 · window 3 ·
// thinking: high" from the structured fields (an LLM snapshot with params;
// thinking off is not shown). Other snapshots (asr: "device · model ·
// precision · language" since phase 3b, no language words) and older
// responses without params keep the back end's preformatted label; an
// empty side is "(none)".
export function modelLabel(m) {
  if (!m) return '';
  // asr (SPEC-v2 I6, gate round 2): an automatic snapshot not bound to a
  // device yet has no device name: "Automatic (waiting for a device) · model
  // · precision · language" from the structured snapshot (SnapshotView.asr)
  const a = m.asr;
  if (a && a.auto && !a.device_id) return [t('common.model.asr_auto_waiting'), a.model, a.precision || a.compute_type, a.language].filter(Boolean).join(' · ');
  const p = m.params;
  if (p && m.model) {
    const parts = [[m.provider_name, m.model].filter(Boolean).join(' / ')];
    // with thinking on the temperature is not sent to the model
    const thinking = p.thinking === 'low' || p.thinking === 'high' || p.thinking === 'max';
    if (typeof p.temperature === 'number' && !thinking) parts.push(t('common.model.temperature', { value: fmtDecimal(p.temperature) }));
    if (typeof p.window === 'number') parts.push(t('common.model.window', { n: p.window }));
    if (p.thinking === 'low') parts.push(t('common.model.thinking_low'));
    else if (p.thinking === 'high') parts.push(t('common.model.thinking_high'));
    else if (p.thinking === 'max') parts.push(t('common.model.thinking_max'));
    return parts.join(' · ');
  }
  if (!m.model && !m.asr && !m.provider_id) return t('common.model.none');
  return m.label || '';
}

// pending … cancelled, and v1.1 A2.1's display-only "outdated".
export function statusName(status) {
  return named('common.status.', status);
}

// v1.1 A2.1: the back end computes display_status (status, or "outdated").
export function displayStatus(st) {
  return st.display_status || st.status;
}

// A done stage that still carries an error text finished with a warning
// (e.g. repair chunks that could not be repaired, NOTES.md Gate B 4). It is
// not a failure: no failure styling, no failure notification.
export function hasWarning(st) {
  return st.status === 'done' && !!st.error;
}

// "Running 42%" for running stages, "Queued · 39% done (checkpoint)" for a
// queued stage that keeps its checkpoint, the plain status name otherwise.
export function stageStatusText(st) {
  if (st.status === 'running') return t('common.status.running_pct', { pct: pct(st.progress) });
  const ck = checkpointProgress(st);
  if (ck != null) return t('common.status.queued_checkpoint', { pct: pct(ck) });
  if (hasWarning(st)) return t('common.status.done_warning');
  return statusName(displayStatus(st));
}

// Short hint for the stages that finished with a warning ("Repair has warnings").
export function warningHint(stages) {
  const names = (stages || []).filter(hasWarning).map((s) => stageName(s.stage));
  return names.length ? t('common.warning_hint', { stages: listJoin(names) }) : '';
}

export function actionName(action) {
  return named('common.action.', action);
}

// Review classes and states (SPEC §6.4, §9.6).
export const CLASS_NAMES = nameTable('common.class.', ['auto', 'absorb', 'decide']);
export const CLASS_HINTS = nameTable('common.class_hint.', ['auto', 'absorb', 'decide']);
export const REVIEW_NAMES = nameTable('common.review_state.', ['none', 'pending', 'accepted', 'rejected', 'edited']);
// Glossary vocabulary (these keys live in the glossary section of the catalogs).
export const SOURCE_NAMES = nameTable('glossary.source.', ['syllabus', 'material', 'review', 'manual']);
// v2 G1: term tiers and the three groups of the glossary (disabled terms
// form their own group whatever their tier).
export const TIER_NAMES = nameTable('glossary.tier.', ['enforce', 'reference']);
// v2 G6 rule 3: where a term's translation came from.
export const ZH_SOURCE_NAMES = nameTable('glossary.zh_source.', ['extractor', 'janitor', 'user']);
// v2 G3.3 janitor actions, as the report shows them.
export const JANITOR_ACTION_NAMES = nameTable('glossary.janitor_action.', ['enforce', 'reference', 'disable', 'merge', 'fix_zh']);

// Diagnostics checks: product names stay as they are; the others are named
// by common.check.{name}.
const CHECK_PRODUCTS = { ffmpeg: 'ffmpeg', python: 'Python', 'faster-whisper': 'faster-whisper', pypdf: 'pypdf', gpu: 'GPU', openvino: 'OpenVINO' };
// SPEC-v2 I10 added openvino_venv, openvino_devices and npu_driver
const CHECK_KEYS = new Set(['data_dir', 'cuda', 'disk', 'openvino_venv', 'openvino_devices', 'npu_driver']);

export function checkName(name) {
  if (CHECK_KEYS.has(name)) return t(`common.check.${name}`);
  if (CHECK_PRODUCTS[name]) return CHECK_PRODUCTS[name];
  if (name.startsWith('api_key:')) return t('common.check.api_key', { name: name.slice('api_key:'.length) });
  if (name.startsWith('api:')) return t('common.check.api', { name: name.slice('api:'.length) });
  // "role_model:{provider}/{model}": a role's model missing from the provider's list
  if (name.startsWith('role_model:')) {
    const rest = name.slice('role_model:'.length);
    const i = rest.indexOf('/');
    return t('common.check.role_model', { provider: i < 0 ? rest : rest.slice(0, i), model: i < 0 ? '' : rest.slice(i + 1) });
  }
  return name;
}

export const CHECK_STATUS_NAMES = nameTable('common.check_status.', ['ok', 'warn', 'error']);

// Floor, so 0.999 shows as 99% rather than a misleading 100%.
export function pct(p) {
  const v = Math.max(0, Math.min(1, Number(p) || 0));
  return numberFormat(Math.floor(v * 100) / 100, { style: 'percent', maximumFractionDigits: 0 });
}

const WAIT_UPSTREAM = 'upstream:';
const WAIT_MATERIAL_TERMS = 'material_terms:';
const WAIT_RETRY_BACKOFF = 'retry_backoff:';
const WAIT_OFFPEAK = 'offpeak:';
const WAIT_DEVICE_PAUSED = 'device_paused:';
const WAIT_DEVICE_SELFTEST = 'device_selftest:';
const WAIT_POWER_QUEUE = 'power_queue:';
const WAIT_POWER_SWITCH = 'power_switch_pending:';

// ---- transcription devices (SPEC-v2 I6-I10) ----

function deviceLists(sys, res) {
  return [(res && res.devices) || [], (sys && sys.devices) || []];
}

function hasDevices(sys, res) {
  return deviceLists(sys, res).some((l) => l.length > 0);
}

/**
 * Whether the power-aware rules of a machine with a battery apply (power
 * management): the resources snapshot's policy.battery_machine; before the
 * first snapshot with a policy, a known power state with a battery.
 */
export function batteryMachine(res) {
  if (!res) return false;
  if (res.policy) return !!res.policy.battery_machine;
  return !!(res.power && res.power.known && res.power.has_battery);
}

// One device at a time (devices.Place rules 5a–5e): a machine with a
// battery outside the manual mode (policy.mode is the effective mode, so
// "manual" only while plugged in).
function singleActive(res) {
  return !!(res && res.policy && res.policy.battery_machine && res.policy.mode !== 'manual');
}

/**
 * Whether a waiting_for is the power-aware policy's (power_* codes, and a
 * self-test wait under the single-active rule): their texts say "挂起 /
 * 排队：电源感知 - …" themselves, so summaries do not add "queued" to them.
 */
export function isPowerWait(w, res) {
  if (!w) return false;
  return w.startsWith('power_') || (w.startsWith(WAIT_DEVICE_SELFTEST) && singleActive(res));
}

/**
 * What waitingForText takes from a resources snapshot (the devices' names,
 * the power policy's single-active state): views that render waiting texts
 * before the first snapshot redraw them when this changes.
 */
export function waitContextKey(res) {
  if (!res) return '';
  return JSON.stringify([((res && res.devices) || []).map((d) => [d.id, d.label]), singleActive(res), batteryMachine(res)]);
}

/** A device's own name (its label, never translated) from the latest device lists; its id when unknown. */
export function deviceLabel(id, sys, res) {
  for (const list of deviceLists(sys, res)) {
    const d = list.find((x) => x.id === id);
    if (d && d.label) return d.label;
  }
  return id;
}

/** The kind of a device in the interface language (devices.kind.{cuda|igpu|npu|cpu}); '' when unknown. */
export function deviceKindName(kind) {
  return kind ? named('devices.kind.', kind) : '';
}

/** Model and precision of a device or a test result: "large-v3-turbo · fp16" (not translated). */
export function comboText(m, precision) {
  return [m, precision].filter(Boolean).join(' · ');
}

/**
 * A device's inference back end as the resource monitor names it (brand
 * names, not translated): OpenVINO; CUDA for faster-whisper on an NVIDIA
 * card; faster-whisper on the CPU.
 */
export function backendName(d) {
  if (!d) return '';
  if (d.backend === 'openvino') return 'OpenVINO';
  if (d.kind === 'cuda') return 'CUDA';
  return d.backend || '';
}

/**
 * Power management (real-machine check 6.8 item 2): the share of the lecture
 * a queued asr stage already holds in its checkpoint (StageView.checkpoint,
 * set by the back end only after a power interrupt), or null — a normal
 * queued stage, and every other one, has none. progress stays the job's own.
 */
export function checkpointProgress(st) {
  const c = st && st.status === 'queued' ? st.checkpoint : null;
  const p = c ? Number(c.progress) : NaN;
  return p > 0 ? Math.min(1, p) : null;
}

/**
 * How far a stage's bar is filled: the progress of a running stage, the
 * checkpoint of a queued one that keeps it, null otherwise (no fill).
 */
export function barProgress(st) {
  if (!st) return null;
  if (st.status === 'running') return st.progress;
  return checkpointProgress(st);
}

/**
 * The parts of an asr snapshot that was resumed on another device (power
 * management: snapshot parts) when there are two or more and they differ in
 * device, model or precision; null otherwise.
 */
export function asrParts(m) {
  const parts = m && m.asr && Array.isArray(m.asr.parts) ? m.asr.parts : [];
  if (parts.length < 2) return null;
  const key = (p) => [p.device_id, p.model, p.precision].join('|');
  return parts.some((p) => key(p) !== key(parts[0])) ? parts : null;
}

// Text for a waiting_for value (SPEC §9.5, v1.1 A3.1 not_started; the codes
// are model.Wait*). `sys` is the latest GET /api/system response (vram
// numbers); `res` the latest resources snapshot (live ASR slot usage, v1.1 A10).
export function waitingForText(w, sys, res) {
  if (!w) return '';
  if (w === 'not_started') return t('waiting.not_started');
  if (w.startsWith(WAIT_UPSTREAM)) return t('waiting.upstream', { stage: stageName(w.slice(WAIT_UPSTREAM.length)) });
  if (w === 'glossary_seed') return t('waiting.glossary_seed');
  if (w === 'glossary_review') return t('waiting.glossary_review'); // v2 G3.1
  if (w.startsWith(WAIT_MATERIAL_TERMS)) return t('waiting.material_terms', { filename: w.slice(WAIT_MATERIAL_TERMS.length) });
  // SPEC-v2 I8 (phase 3b): with transcription devices, gpu_paused is the
  // main switch over every device and gpu_slot means every device the job
  // may use is busy; the device of device_paused:{id} / device_selftest:{id}
  // is named by its own label (resources or system device lists).
  // (before the first system / resources response the device wording is
  // used too: every 3b back end has devices; a back end without them sends
  // neither device lists nor the power state)
  const withDevices = (!sys && !res) || hasDevices(sys, res) || !!(res && res.power) || !!(sys && sys.power);
  if (w === 'gpu_paused') return withDevices ? t('waiting.all_paused') : t('waiting.gpu_paused');
  if (w.startsWith(WAIT_DEVICE_PAUSED)) return t('waiting.device_paused', { device: deviceLabel(w.slice(WAIT_DEVICE_PAUSED.length), sys, res) });
  if (w.startsWith(WAIT_DEVICE_SELFTEST)) {
    // on a machine with a battery the job waits for the test because only
    // one device is active at a time (power-aware, requirement 7)
    const device = deviceLabel(w.slice(WAIT_DEVICE_SELFTEST.length), sys, res);
    return singleActive(res) ? t('waiting.power_device_selftest', { device }) : t('waiting.device_selftest', { device });
  }
  if (w === 'on_battery') return t('waiting.on_battery'); // before power management (old data)
  // power management: the power-aware waits (each says so, requirement 7);
  // device ids are shown by the devices' own names
  if (w === 'power_wait_ac') return t('waiting.power_wait_ac');
  if (w === 'power_no_saver_device') return t('waiting.power_no_saver_device');
  if (w === 'power_no_device') return t('waiting.power_no_device');
  if (w.startsWith(WAIT_POWER_QUEUE)) return t('waiting.power_queue', { device: deviceLabel(w.slice(WAIT_POWER_QUEUE.length), sys, res) });
  if (w.startsWith(WAIT_POWER_SWITCH)) {
    // power_switch_pending:{from}:{to}, each id with one colon (model.ParseWaitPowerSwitchPending)
    const p = w.slice(WAIT_POWER_SWITCH.length).split(':');
    if (p.length === 4 && p.every(Boolean)) return t('waiting.power_switch_pending', { from: deviceLabel(`${p[0]}:${p[1]}`, sys, res), to: deviceLabel(`${p[2]}:${p[3]}`, sys, res) });
  }
  if (w === 'memory') return t('waiting.memory');
  if (w === 'no_asr_device') return t('waiting.no_asr_device');
  if (w === 'gpu_slot' && withDevices) return t('waiting.device_slot');
  if (w === 'gpu_slot') {
    const r = res && res.gpu;
    if (r && r.asr_slots_max) return t('waiting.gpu_slot_count', { used: r.asr_slots_used, max: r.asr_slots_max });
    const g = sys && sys.gpu;
    return g ? t('waiting.gpu_slot_count', { used: g.slots_used, max: g.slots_max }) : t('waiting.gpu_slot');
  }
  if (w === 'vram') {
    const g = sys && sys.gpu;
    if (!g) return t('waiting.vram');
    // The scheduler needs free - vram_reserve_mb >= estimate (SPEC §5), so
    // the reserve is part of what is "needed" when the config exposes it.
    const reserve = Number(sys.config && sys.config.asr && sys.config.asr.vram_reserve_mb) || 0;
    return t('waiting.vram_detail', { free: fmtInt(g.free_mb), need: fmtInt(g.estimated_job_mb + reserve) });
  }
  if (w === 'cpu_slot') return t('waiting.cpu_slot'); // added by lead (model.WaitCPUSlot)
  if (w === 'api_slot') return t('waiting.api_slot');
  if (w === 'budget') return t('waiting.budget');
  if (w.startsWith(WAIT_RETRY_BACKOFF)) {
    const raw = w.slice(WAIT_RETRY_BACKOFF.length);
    const n = Number(raw);
    return t('waiting.retry_backoff', { n: raw !== '' && Number.isFinite(n) ? n : raw });
  }
  // v2 P12.6: offpeak:{local HH:MM} — the provider only takes requests off-peak
  if (w.startsWith(WAIT_OFFPEAK)) return t('waiting.offpeak', { time: w.slice(WAIT_OFFPEAK.length) });
  return w;
}

// ---- coded back-end texts (SPEC-v2 P14.3) ----

// Param values that are codes the front end names itself (codes-p2
// conventions in testdata/api/error_codes_backend.json): role and stage ids,
// job statuses, playback-file states; sizes by their unit suffix (*_bytes,
// *_mb). Other lists are joined as a list (e.g. the missing regex groups);
// a nested coded text ({code, params, detail}, e.g. the reason of a
// calendar.file_invalid problem) is rendered in the same namespace first.
// Everything else is interpolated as is.
// Integer params shown with the language's digit grouping, by code (other
// integers may be years, ports or ids: left as they are).
const GROUPED_INT_PARAMS = { 'check.gpu_vram': ['total', 'free'] };

function paramValue(ns, name, v, code) {
  if (name === 'role' && typeof v === 'string') return roleName(v);
  if (name === 'roles' && Array.isArray(v)) return listJoin(v.map(roleName));
  if (name === 'stage' && typeof v === 'string') return stageName(v);
  if (name === 'stages' && Array.isArray(v)) return listJoin(v.map(stageName));
  if (name === 'status' && typeof v === 'string') return statusName(v);
  if (name === 'stream_status' && typeof v === 'string') return named('common.stream_status.', v);
  if (typeof v === 'number' && isFinite(v)) {
    if (name.endsWith('_bytes')) return fmtBytes(v);
    if (name.endsWith('_mb')) return fmtMB(v);
    // decimals with the language's decimal sign (qa.length_ratio {ratio} 0.38, {min} 0.5)
    if (!Number.isInteger(v)) return fmtDecimal(v, 2);
    if ((GROUPED_INT_PARAMS[code] || []).includes(name)) return fmtInt(v);
  }
  if (Array.isArray(v) && v.every((x) => x == null || typeof x !== 'object')) return listJoin(v.map(String));
  if (v && typeof v === 'object' && !Array.isArray(v) && typeof v.code === 'string') return codedText(ns, v, '');
  return v;
}

/**
 * The text of a coded back-end text (ErrorInfo {code, params, detail}):
 * t(ns + "." + code, params) with ns "errors" | "notes" | "warnings".
 * params.extra ([]ErrorInfo, compound notes) are rendered in order and
 * joined as clauses. Without a code, or when the catalog has no such key,
 * the detail, then `fallback`, then the code itself.
 */
export function codedText(ns, info, fallback = '') {
  if (!info || !info.code) return (info && info.detail) || fallback || '';
  const key = `${ns}.${info.code}`;
  if (!has(key)) return info.detail || fallback || info.code;
  const raw = info.params || {};
  const params = {};
  for (const [k, v] of Object.entries(raw)) if (k !== 'extra') params[k] = paramValue(ns, k, v, info.code);
  const main = t(key, params);
  const extra = Array.isArray(raw.extra) ? raw.extra.map((x) => codedText(ns, x, '')).filter(Boolean) : [];
  return extra.length ? clauseJoin([main, ...extra]) : main;
}

/** A coded error (HTTP error body, job error, batch item, balance, provider test). */
export function errorInfoText(info, fallback = '') {
  return codedText('errors', info, fallback);
}

/** A stage's error: its coded warning (done with a warning) or error, else the text. */
export function stageErrorText(st) {
  if (!st) return '';
  if (hasWarning(st) && st.warning_info) return codedText('warnings', st.warning_info, st.error || '');
  if (st.error_info) return codedText('errors', st.error_info, st.error || '');
  return st.error || '';
}

/** A stage's note (job.stats.note_info, else the note text). */
export function noteText(st) {
  if (!st) return '';
  return st.note_info ? codedText('notes', st.note_info, st.note || '') : st.note || '';
}

/**
 * One stale reason {code, params} (model.StaleCode*), like the back end's
 * state.StaleReasonText: a glossary reason without a recorded "from" shows
 * the current version only; a legacy reason shows its original text.
 */
export function staleReasonText(r) {
  if (!r || !r.code) return t('stale.reason.input_changed');
  const p = r.params || {};
  switch (r.code) {
    case 'glossary_updated':
    case 'glossary_janitor':
      if (p.to == null) return t(`stale.reason.${r.code}_plain`);
      if (p.from == null) return t(`stale.reason.${r.code}_now`, { to: p.to });
      return t(`stale.reason.${r.code}`, { from: p.from, to: p.to });
    case 'material_changed':
      return p.filename ? t('stale.reason.material_changed', { filename: p.filename }) : t('stale.reason.material_changed_plain');
    case 'materials_changed': {
      // lecture kinds: the slides a lecture uses (its own, its week's,
      // course-wide) changed; added / removed are file-name lists
      const added = Array.isArray(p.added) && p.added.length ? listJoin(p.added.map(String)) : '';
      const removed = Array.isArray(p.removed) && p.removed.length ? listJoin(p.removed.map(String)) : '';
      if (added && removed) return t('stale.reason.materials_changed', { added, removed });
      if (added) return t('stale.reason.materials_changed_added', { added });
      if (removed) return t('stale.reason.materials_changed_removed', { removed });
      return t('stale.reason.materials_changed_plain');
    }
    case 'legacy':
      return p.text != null && String(p.text).trim() ? t('stale.reason.legacy', { text: String(p.text) }) : t('stale.reason.input_changed');
  }
  return has(`stale.reason.${r.code}`) ? t(`stale.reason.${r.code}`, p) : t('stale.reason.input_changed');
}

/** A stale stage's reason: stale_info, else the transitional stale_reason text. */
export function stageStaleText(st) {
  if (!st) return '';
  if (st.stale_info) return staleReasonText(st.stale_info);
  return st.stale_reason || '';
}

/**
 * Lecture.stale {reasons, stages} as the row text of v1.1 A2.2
 * ("{reasons}, needs updating: {stages}"); "" when nothing is stale.
 */
export function staleSummaryText(stale) {
  if (!stale || !(stale.stages || []).length) return '';
  const reasons = [...new Set((stale.reasons || []).map(staleReasonText))];
  if (!reasons.length) reasons.push(t('stale.reason.input_changed'));
  return t('stale.need_update', { reasons: clauseJoin(reasons), stages: listJoin(stale.stages.map(stageName)) });
}

/** A lecture's stale text: Lecture.stale, else the transitional stale_text. */
export function lectureStaleText(lec) {
  if (!lec) return '';
  if (lec.stale) return staleSummaryText(lec.stale);
  return lec.stale_text || '';
}

export function isAllDone(stages) {
  return Array.isArray(stages) && stages.length > 0 && stages.every((s) => s.status === 'done');
}

// A failed stage is final (no automatic retry left) when the back end does
// not report a pending retry through waiting_for (retry_backoff:{s}).
export function isFinalFailure(st) {
  return st.status === 'failed' && !st.waiting_for;
}

// One-line summary of a pipeline, worst state first.
// tone: ok | run | warn | err | muted. `extra` = {sys, res, staleText, stale,
// draft} (v1.1: the lecture's stale_text and draft flag; v2 P14.3: stale is
// Lecture.stale, used when staleText is not given); older callers pass sys only.
export function lectureSummary(stages, extra) {
  const sum = pipelineSummary(stages, extra);
  // a queued stage the power policy put back in line keeps its checkpoint:
  // "{summary} · 39% done (checkpoint)" (StageView.checkpoint)
  const ck = sum.kind === 'queued' ? checkpointProgress(sum.stage) : null;
  if (ck != null) sum.text = t('common.summary.checkpoint', { summary: sum.text, pct: pct(ck) });
  return sum;
}

function pipelineSummary(stages, extra) {
  const isOpts = extra && ('sys' in extra || 'res' in extra || 'staleText' in extra || 'stale' in extra || 'draft' in extra);
  const opt = isOpts ? extra : { sys: extra };
  if (!Array.isArray(stages) || stages.length === 0) return { text: t('common.summary.none'), tone: 'muted', kind: 'none' };
  if (opt.draft) return { text: t('common.summary.draft'), tone: 'muted', kind: 'draft' };
  const find = (status) => stages.find((s) => s.status === status);
  const wait = (st) => (st.waiting_for ? waitingForText(st.waiting_for, opt.sys, opt.res) : '');
  let st;
  if ((st = find('failed'))) {
    const w = wait(st);
    const stage = stageName(st.stage);
    return { text: w ? t('common.summary.failed_wait', { stage, wait: w }) : t('common.summary.failed', { stage }), tone: 'err', kind: 'failed', stage: st };
  }
  if ((st = find('running'))) return { text: t('common.summary.running', { stage: stageName(st.stage) }), tone: 'run', kind: 'running', stage: st };
  if ((st = stages.find((s) => s.status === 'queued' && s.error && s.waiting_for && s.waiting_for.startsWith(WAIT_RETRY_BACKOFF)))) {
    return { text: t('common.summary.failed_wait', { stage: stageName(st.stage), wait: wait(st) }), tone: 'warn', kind: 'retrying', stage: st };
  }
  if ((st = find('stale'))) {
    const text = opt.staleText || (opt.stale ? staleSummaryText(opt.stale) : '') || t('common.summary.stale', { stage: stageName(st.stage) });
    return { text, tone: 'warn', kind: 'stale', stage: st };
  }
  if ((st = find('cancelled'))) return { text: t('common.summary.cancelled', { stage: stageName(st.stage) }), tone: 'muted', kind: 'cancelled', stage: st };
  if ((st = find('queued'))) {
    const w = wait(st);
    const stage = stageName(st.stage);
    // a power-aware wait names its state itself ("挂起：电源感知 - …"):
    // "{stage}：{wait}", never "{stage}排队中：挂起：…"
    if (w && isPowerWait(st.waiting_for, opt.res)) return { text: t('common.summary.pending', { stage, wait: w }), tone: 'muted', kind: 'queued', stage: st };
    return { text: w ? t('common.summary.queued_wait', { stage, wait: w }) : t('common.summary.queued', { stage }), tone: 'muted', kind: 'queued', stage: st };
  }
  if ((st = find('pending'))) return { text: t('common.summary.pending', { stage: stageName(st.stage), wait: wait(st) || statusName('pending') }), tone: 'muted', kind: 'pending', stage: st };
  return { text: statusName('done'), tone: 'ok', kind: 'done' };
}

// The running stage, if any (its progress is shown next to the bar).
export function runningStage(stages) {
  return (stages || []).find((s) => s.status === 'running') || null;
}

const isBusy = (s) => s.status === 'pending' || s.status === 'queued' || s.status === 'running';

// True while any term extraction job of the course is unfinished (SPEC §9.3
// "extracting…"); the v2 glossary_review is not an extraction (reviewBusy).
export function extractionBusy(course) {
  if (!course) return false;
  if ((course.stages || []).some((s) => s.stage !== 'glossary_review' && isBusy(s))) return true;
  return (course.materials || []).some((m) => (m.stages || []).some(isBusy));
}

// The course's glossary_review stage (v2 G3.1), or null.
export function reviewStage(course) {
  return (course && (course.stages || []).find((s) => s.stage === 'glossary_review')) || null;
}

// True while the glossary janitor waits, is queued or runs.
export function reviewBusy(course) {
  const st = reviewStage(course);
  return !!st && isBusy(st);
}

// What keeps a course's glossary from being ready (glossary_pending, A3.2):
// extracting terms / reviewing the glossary / "" when nothing.
export function glossaryBusyText(course) {
  if (!course) return '';
  if (extractionBusy(course)) return t('common.glossary_busy.extracting');
  if (reviewBusy(course)) return t('common.glossary_busy.reviewing');
  return course.glossary_pending ? t('common.glossary_busy.pending') : '';
}

// The course or material stage a job event refers to, or null.
export function findCourseStage(courses, ev) {
  for (const c of courses || []) {
    if (ev.scope === 'course' && c.id === ev.scope_id) return (c.stages || []).find((s) => s.stage === ev.stage) || null;
    if (ev.scope === 'material') {
      const m = (c.materials || []).find((x) => x.id === ev.scope_id);
      if (m) return (m.stages || []).find((s) => s.stage === ev.stage) || null;
    }
  }
  return null;
}

// ---- formatting (SPEC-v2 P14.4) ----
//
// The one set of formatters of the whole UI (lead ruling at the gate): views
// format numbers, sizes, money, dates and relative times only through these,
// never with i18n.js's fmt* or toLocaleString. Everything follows the UI
// language through Intl (compact notation "1.8M" / "90.1万", byte units,
// currencies, DateTimeFormat, RelativeTimeFormat). Durations take their
// words from the catalog; fmtClock is language-neutral.

// UI languages whose formatting data a browser may lack, with the closest
// locales to use instead, in order. Chrome ships no yue data: Intl formats
// yue-Hant-HK as en-GB ("29 Sept 2026", "1.84m"); zh-Hant-HK is Hong Kong
// Chinese with the same dates and numbers (NOTES p2-tail). A browser that
// has the language's data uses it.
const INTL_FALLBACK = { 'yue-Hant-HK': ['zh-Hant-HK', 'zh-HK', 'zh-TW'] };
const intlResolved = new Map(); // UI language → the Intl locale used

function intlSupports(loc) {
  try {
    return Intl.DateTimeFormat.supportedLocalesOf([loc]).length > 0 && Intl.NumberFormat.supportedLocalesOf([loc]).length > 0;
  } catch {
    return false;
  }
}

/**
 * The Intl locale of the UI language: en-XA formats as English; en dates
 * use en-GB's day-month order, which cannot be misread (P14.1); a language
 * the browser has no data for uses INTL_FALLBACK (yue-Hant-HK → zh-Hant-HK).
 */
export function intlLocale(forDates = false) {
  const loc = getLocale();
  if (loc === 'en' || loc === PSEUDO) return forDates ? 'en-GB' : 'en';
  let r = intlResolved.get(loc);
  if (r === undefined) {
    r = intlSupports(loc) ? loc : (INTL_FALLBACK[loc] || []).find(intlSupports) || loc;
    intlResolved.set(loc, r);
  }
  return r;
}

// Intl formatters are reused: the views format many numbers per render.
const formatters = new Map();

function formatter(kind, locale, opts) {
  const key = `${kind}|${locale}|${JSON.stringify(opts)}`;
  let f = formatters.get(key);
  if (!f) {
    if (kind === 'number') f = new Intl.NumberFormat(locale, opts);
    else if (kind === 'date') f = new Intl.DateTimeFormat(locale, opts);
    else f = new Intl.RelativeTimeFormat(locale, opts);
    formatters.set(key, f);
  }
  return f;
}

function numberFormat(n, opts = {}) {
  return formatter('number', intlLocale(), opts).format(n);
}

function dateFormat(d, opts) {
  return formatter('date', intlLocale(true), opts).format(d);
}

function toDate(d) {
  const date = d instanceof Date ? d : new Date(d);
  return isNaN(date.getTime()) ? null : date;
}

// A whole number with the language's digit grouping: 12,345.
export function fmtInt(n) {
  return numberFormat(Math.round(Number(n) || 0), { maximumFractionDigits: 0 });
}

/**
 * A count for t(): params.n of a plural text is shown with the language's
 * digit grouping (fmtInt) while the plural rules still see the number,
 * e.g. t('lecture.stat.total', { n: count(1234) }) → "共 1,234 句".
 */
export function count(n) {
  const v = Number(n) || 0;
  return { valueOf: () => v, toString: () => fmtInt(v) };
}

// A plain decimal (playback rate 1.25, price multiplier 0.5): at most
// maxDigits fraction digits, with the language's decimal sign.
export function fmtDecimal(n, maxDigits = 2) {
  if (n == null || !isFinite(n)) return '';
  return numberFormat(Number(n), { maximumFractionDigits: maxDigits });
}

// A date with its weekday: "Friday 23 Oct" (en).
export function fmtDayWithWeekday(date = new Date()) {
  const d = toDate(date);
  return d ? dateFormat(d, { weekday: 'long', day: 'numeric', month: 'short' }) : '';
}

// A calendar day without the year: "6 Oct" (en).
export function fmtMonthDay(date) {
  const d = toDate(date);
  return d ? dateFormat(d, { month: 'short', day: 'numeric' }) : '';
}

// A calendar day with its short weekday, and the time of day when withTime,
// in the order of the UI language ("Fri 23 Oct", "Fri 23 Oct, 14:00" in en):
// never glue fmtMonthDay, fmtWeekday and a time together.
export function fmtDayShort(date, withTime = false) {
  const d = toDate(date);
  if (!d) return '';
  const opts = { weekday: 'short', month: 'short', day: 'numeric' };
  // another year (a past lecture recorded in 2024) says which one
  if (d.getFullYear() !== new Date().getFullYear()) opts.year = 'numeric';
  if (withTime) Object.assign(opts, { hour: '2-digit', minute: '2-digit', hourCycle: 'h23' });
  return dateFormat(d, opts);
}

// Month and year ("October 2024", zh 2024年10月): when a past lecture was recorded.
export function fmtMonthYear(date) {
  const d = toDate(date);
  return d ? dateFormat(d, { year: 'numeric', month: 'long' }) : '';
}

// The short month name alone ("Oct"): month labels of the calendar.
export function fmtMonth(date) {
  const d = toDate(date);
  return d ? dateFormat(d, { month: 'short' }) : '';
}

// The weekday name; style "long" | "short" | "narrow".
export function fmtWeekday(date, style = 'short') {
  const d = toDate(date);
  return d ? dateFormat(d, { weekday: style }) : '';
}

// The seven weekday names from Monday (weeks run Monday to Sunday, P13.4).
export function weekdayNames(style = 'short') {
  return Array.from({ length: 7 }, (_, i) => fmtWeekday(new Date(2024, 0, 1 + i), style)); // 2024-01-01 is a Monday
}

// "14:05" on a 24-hour clock in the IANA zone tz (the computer's own zone
// when tz is empty): tariff times are shown and compared in this form.
export function fmtHM(date, tz) {
  const d = toDate(date);
  if (!d) return '';
  const opts = { hour: '2-digit', minute: '2-digit', hourCycle: 'h23' };
  if (tz) opts.timeZone = tz;
  return dateFormat(d, opts);
}

// "14:05:09" on a 24-hour clock (sampling times of the resource panel).
export function fmtHMS(date) {
  const d = toDate(date);
  return d ? dateFormat(d, { hour: '2-digit', minute: '2-digit', second: '2-digit', hourCycle: 'h23' }) : '';
}

// 51200 -> "00:51", 3723000 -> "1:02:03"
export function fmtClock(ms) {
  const total = Math.max(0, Math.floor((Number(ms) || 0) / 1000));
  const h = Math.floor(total / 3600);
  const m = Math.floor((total % 3600) / 60);
  const s = total % 60;
  const two = (n) => String(n).padStart(2, '0');
  return h > 0 ? `${h}:${two(m)}:${two(s)}` : `${two(m)}:${two(s)}`;
}

export function fmtDuration(ms) {
  if (ms == null || !isFinite(ms)) return '';
  const sec = Math.round(ms / 1000);
  if (sec < 1) return t('common.duration.lt1s');
  if (sec < 60) return t('common.duration.s', { s: sec });
  if (sec < 3600) return t('common.duration.ms', { m: Math.floor(sec / 60), s: sec % 60 });
  return t('common.duration.hm', { h: Math.floor(sec / 3600), m: Math.floor((sec % 3600) / 60) });
}

// A real-time factor (transcription time ÷ audio length): three fraction
// digits below 1 ("0.034"), two from 1 on ("1.6").
export function fmtRTF(rtf) {
  if (rtf == null || !isFinite(rtf)) return '';
  return numberFormat(Number(rtf), { maximumFractionDigits: rtf < 1 ? 3 : 2 });
}

/**
 * The hover-card texts of an asr stage's asr_timing (phase 3b): time is
 * the transcription time with the audio length, speed the RTF with the
 * speed-up ("RTF 0.034 (about 29× real time)" in en), load the model load time (noting a
 * first compile on the device). A part without its numbers is ''.
 */
export function asrTimingText(tm) {
  const out = { time: '', speed: '', load: '' };
  if (!tm) return out;
  const tr = Number(tm.transcribe_ms) || 0;
  const audio = Number(tm.audio_ms) || 0;
  if (tr > 0) out.time = audio > 0 ? t('common.pipe.asr_time', { time: fmtDuration(tr), audio: fmtDuration(audio) }) : fmtDuration(tr);
  if (tr > 0 && audio > 0) {
    const speed = audio / tr;
    out.speed = t('common.pipe.rtf', { rtf: fmtRTF(tr / audio), speed: fmtDecimal(speed, speed >= 10 ? 0 : 1) });
  }
  const load = Number(tm.load_ms) || 0;
  if (tm.first_compile) out.load = t('common.pipe.load_first_compile', { time: fmtDuration(load) });
  else if (load > 0) out.load = fmtDuration(load);
  return out;
}

// Token counts in the compact notation of the UI language with three
// significant digits: 901K, 1.84M (en). Numbers the language does not
// abbreviate (below ten thousand in Chinese) keep all their digits.
export function fmtTokens(n) {
  n = Number(n) || 0;
  const f = formatter('number', intlLocale(), { notation: 'compact', maximumSignificantDigits: 3 });
  const parts = f.formatToParts(n);
  return parts.some((p) => p.type === 'compact') ? parts.map((p) => p.value).join('') : fmtInt(n);
}

// An estimated cost without a currency: 4 decimals under 0.01, else 2.
export function fmtCost(c) {
  if (c == null || !isFinite(c)) return '';
  const digits = c < 0.01 ? 4 : 2;
  return numberFormat(c, { minimumFractionDigits: digits, maximumFractionDigits: digits });
}

// Byte units of the UI language (byte, kB, MB, GB).
function unitFormat(n, unit, digits) {
  return numberFormat(n, { style: 'unit', unit, unitDisplay: 'short', maximumFractionDigits: digits });
}

export function fmtBytes(n) {
  n = Number(n) || 0;
  if (n < 1024) return unitFormat(n, 'byte', 0);
  if (n < 1024 * 1024) return unitFormat(n / 1024, 'kilobyte', 1);
  if (n < 1024 * 1024 * 1024) return unitFormat(n / 1024 / 1024, 'megabyte', 1);
  return unitFormat(n / 1024 / 1024 / 1024, 'gigabyte', 2);
}

export function fmtMB(mb) {
  mb = Number(mb) || 0;
  return mb >= 1024 ? unitFormat(mb / 1024, 'gigabyte', 1) : unitFormat(mb, 'megabyte', 0);
}

// A calendar date in the current language, "12 Oct" style; the year only
// when asked (en uses the unambiguous day-month order, P14.1).
function dateText(d, withYear) {
  const opts = { month: 'short', day: 'numeric' };
  if (withYear) opts.year = 'numeric';
  return dateFormat(d, opts);
}

// RFC3339 -> now / 3 minutes ago / 2 hours ago (Intl.RelativeTimeFormat),
// today 09:12 / yesterday 14:20 (the catalog's words around the Intl time),
// else a date (with the year when it is not this year).
export function relTime(iso, now = Date.now()) {
  if (!iso) return '';
  const d = new Date(iso);
  const ms = d.getTime();
  if (isNaN(ms)) return iso;
  const s = Math.round((now - ms) / 1000);
  const rtf = formatter('relative', intlLocale(), { numeric: 'auto' });
  if (s < 45) return rtf.format(0, 'second');
  if (s < 3600) return rtf.format(-Math.max(1, Math.round(s / 60)), 'minute');
  const today = new Date(now);
  today.setHours(0, 0, 0, 0);
  if (ms >= today.getTime() && s < 6 * 3600) return rtf.format(-Math.round(s / 3600), 'hour');
  const time = () => dateFormat(d, { hour: '2-digit', minute: '2-digit', hourCycle: 'h23' }); // 24-hour, like every time of the UI
  if (ms >= today.getTime()) return t('common.time.today', { time: time() });
  if (ms >= today.getTime() - 86400000) return t('common.time.yesterday', { time: time() });
  return dateText(d, d.getFullYear() !== today.getFullYear());
}

// 2.1415, "CNY" -> Intl's currency format of the provider's currency
// (P14.4 keeps the currency the provider reports; RMB is CNY); an unknown
// code -> "2.14 XYZ"; none -> "2.14".
export function fmtMoney(amount, currency) {
  if (amount == null || !isFinite(amount)) return '';
  const digits = amount > 0 && amount < 0.01 ? 4 : 2;
  const plain = { minimumFractionDigits: digits, maximumFractionDigits: digits };
  if (!currency) return numberFormat(amount, plain);
  const code = currency.toUpperCase() === 'RMB' ? 'CNY' : currency.toUpperCase();
  try {
    return numberFormat(amount, { ...plain, style: 'currency', currency: code });
  } catch {
    return `${numberFormat(amount, plain)} ${currency}`;
  }
}

// A temperature in degrees Celsius ("72 °C", "72°" when narrow: the resource rail).
export function fmtCelsius(c, narrow = false) {
  if (c == null || !isFinite(c)) return '';
  return numberFormat(Number(c), { style: 'unit', unit: 'celsius', unitDisplay: narrow ? 'narrow' : 'short', maximumFractionDigits: 0 });
}

export function fmtPercent(rate, digits = 1) {
  if (rate == null || !isFinite(rate)) return '';
  return numberFormat(rate, { style: 'percent', minimumFractionDigits: digits, maximumFractionDigits: digits });
}

// Course code + lecture label: "CSC1003 L03" (label only without a code).
export function lectureRef(code, label) {
  return code ? `${code} ${label}` : label;
}

// RFC3339 -> local date and time: "23 Oct 2026, 20:00" (en).
export function fmtDateTime(iso) {
  if (!iso) return '';
  const d = new Date(iso);
  if (isNaN(d.getTime())) return iso;
  return dateFormat(d, { year: 'numeric', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', hourCycle: 'h23' });
}

export function cacheHitRate(u) {
  if (!u || !u.prompt_tokens) return null;
  return u.cache_hit_tokens / u.prompt_tokens;
}

// "Input 901k (cache hits 92.1%) · Output 42.9k · Est. 0.12"
export function usageText(u) {
  if (!u) return '';
  const parts = [];
  const rate = cacheHitRate(u);
  const tokens = fmtTokens(u.prompt_tokens);
  parts.push(rate != null ? t('common.usage.input_cached', { tokens, rate: fmtPercent(rate, 1) }) : t('common.usage.input', { tokens }));
  parts.push(t('common.usage.output', { tokens: fmtTokens(u.completion_tokens) }));
  if (u.estimated_cost != null) parts.push(t('common.usage.cost', { cost: fmtCost(u.estimated_cost) }));
  return parts.join(' · ');
}

export function hasUsage(u) {
  return !!u && (u.prompt_tokens > 0 || u.completion_tokens > 0);
}

// First n characters (code points, not UTF-16 units) of an error text.
export function excerpt(text, n = 300) {
  const chars = Array.from(text || '');
  return chars.length <= n ? chars.join('') : chars.slice(0, n).join('') + '…';
}

export function stripExt(name) {
  const i = name.lastIndexOf('.');
  return i > 0 ? name.slice(0, i) : name;
}

export function extOf(name) {
  const i = name.lastIndexOf('.');
  return i > 0 ? name.slice(i + 1).toLowerCase() : '';
}

// Files dropped together whose names differ only by extension (SPEC §9.4.3).
export function sameStemGroups(files) {
  const groups = new Map();
  for (const f of files) {
    const key = stripExt(f.name).toLowerCase();
    if (!groups.has(key)) groups.set(key, []);
    groups.get(key).push(f);
  }
  return [...groups.values()].filter((g) => g.length > 1);
}

const MEDIA_EXTS = new Set([
  'mp4', 'm4v', 'mkv', 'mov', 'webm', 'avi', 'wmv', 'flv', 'ts', 'mts', 'mpg', 'mpeg', '3gp',
  'mp3', 'm4a', 'aac', 'wav', 'flac', 'ogg', 'oga', 'opus', 'wma', 'amr', 'aiff', 'aif',
]);

export function isMediaFile(file) {
  const type = file.type || '';
  return type.startsWith('audio/') || type.startsWith('video/') || MEDIA_EXTS.has(extOf(file.name));
}

export function isSyllabusFile(file) {
  return ['txt', 'md', 'pdf'].includes(extOf(file.name));
}

export function isPDF(file) {
  return extOf(file.name) === 'pdf';
}

// ---- word diff (SPEC §9.6, §9.7) ----

export function tokenize(s) {
  return (s || '').split(/\s+/).filter(Boolean);
}

const DIFF_CELL_LIMIT = 250000;

// Word-level diff of a -> b. Returns runs [{t: 'eq'|'del'|'ins', w: 'words'}],
// consecutive tokens of the same kind merged. Within a changed region the
// deletions come before the insertions.
export function wordDiff(a, b) {
  const A = tokenize(a);
  const B = tokenize(b);
  const n = A.length;
  const m = B.length;
  const ops = [];
  const push = (t, w) => {
    const last = ops[ops.length - 1];
    if (last && last.t === t) last.w += ' ' + w;
    else ops.push({ t, w });
  };
  if ((n + 1) * (m + 1) > DIFF_CELL_LIMIT) {
    if (n) push('del', A.join(' '));
    if (m) push('ins', B.join(' '));
    return ops;
  }
  // L[i*(m+1)+j] = length of the LCS of A[i:] and B[j:]
  const W = m + 1;
  const L = new Int32Array((n + 1) * W);
  for (let i = n - 1; i >= 0; i--) {
    for (let j = m - 1; j >= 0; j--) {
      L[i * W + j] = A[i] === B[j] ? L[(i + 1) * W + j + 1] + 1 : Math.max(L[(i + 1) * W + j], L[i * W + j + 1]);
    }
  }
  let i = 0;
  let j = 0;
  while (i < n && j < m) {
    if (A[i] === B[j]) {
      push('eq', A[i]);
      i++;
      j++;
    } else if (L[(i + 1) * W + j] >= L[i * W + j + 1]) {
      push('del', A[i]);
      i++;
    } else {
      push('ins', B[j]);
      j++;
    }
  }
  while (i < n) push('del', A[i++]);
  while (j < m) push('ins', B[j++]);
  return ops;
}
