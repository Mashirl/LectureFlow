// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// HTTP calls of the onboarding (SPEC-v3 V4) and of the component manager
// (V3, .claude/v3-brief.md §7.2-§7.4) that api.js does not have yet. Same
// conventions as api.js: JSON in and out, failures become ApiError (shown
// through the page's error handler unless opts.quiet), mock mode appends
// ?mock=… to every request.

import { api, ApiError, errorText, MOCK, MOCK_SCENARIO } from './api.js';
import { t } from './i18n.js';

function withMock(path) {
  if (!MOCK) return path;
  return path + (path.includes('?') ? '&' : '?') + 'mock=' + encodeURIComponent(MOCK_SCENARIO);
}

let onError = null;

/** The handler for failed requests that are not quiet (the onboarding page shows a toast). */
export function setErrorHandler(fn) {
  onError = fn;
}

async function request(method, path, body, opts = {}) {
  const init = { method, cache: 'no-store', headers: {} };
  if (body !== undefined) {
    init.headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(body);
  }
  const fail = (err) => {
    if (!opts.quiet && onError) onError(err);
    return err;
  };
  let res;
  try {
    res = await fetch(withMock(path), init);
  } catch {
    throw fail(new ApiError(0, t('common.net.unreachable')));
  }
  const text = await res.text().catch(() => '');
  let parsed = null;
  try {
    parsed = text ? JSON.parse(text) : null;
  } catch {
    parsed = null;
  }
  if (!res.ok) throw fail(new ApiError(res.status, errorText(res.status, parsed), parsed));
  if (text && parsed === null && text.trim() !== 'null') throw fail(new ApiError(res.status, t('common.net.bad_response')));
  return parsed;
}

const get = (p, o) => request('GET', p, undefined, o);
const post = (p, b, o) => request('POST', p, b, o);
const put = (p, b, o) => request('PUT', p, b, o);
const enc = encodeURIComponent;

export const obApi = {
  app: (o) => get('/api/app', o), // AppInfo
  onboarding: (o) => get('/api/onboarding', o), // OnboardingState {step, done, data}
  saveOnboarding: (state, o) => put('/api/onboarding', state, o), // OnboardingState
  envKeys: (o) => get('/api/onboarding/env-keys', o), // [{provider_type, env, present, masked}]
  useEnvKey: (providerType, o) => post('/api/onboarding/use-env-key', { provider_type: providerType }, o), // ProviderView
  hardware: (o) => get('/api/hardware', o), // {hw, recommendation}
  detectHardware: (o) => post('/api/hardware/detect', undefined, o), // {hw, recommendation}
  components: (o) => get('/api/components', o), // {platform, manifest, components, tasks}
  plan: (ids, o) => post('/api/components/plan', { ids }, o), // {ids, download_bytes, install_bytes, free_bytes, enough_space, need_bytes}
  install: (ids, o) => post('/api/components/install', { ids }, o), // 202 {task}
  tasks: (o) => get('/api/components/tasks', o), // [TaskStatus]
  taskAction: (id, action, o) => post(`/api/components/tasks/${enc(id)}/${enc(action)}`, undefined, o), // {task}
  sources: (o) => get('/api/components/sources', o), // Sources
  saveSources: (s, o) => put('/api/components/sources', { pypi: s.pypi, models: s.models }, o), // Sources
  presets: (o) => get('/api/components/presets', o), // Presets
  startImport: (path, o) => post('/api/app/import', { path }, o), // 202 import status
  // The import status: GET /api/app/import, or GET /api/app/data-dir where
  // the server has no such route (404). Resolves to {state, copied_bytes, total_bytes, error?}.
  async importStatus(o = {}) {
    try {
      return await get('/api/app/import', { quiet: true });
    } catch (e) {
      if (e.status !== 404) {
        if (!o.quiet && onError) onError(e);
        throw e;
      }
    }
    const d = await get('/api/app/data-dir', o);
    return (d && (d.import || d.move || d)) || null;
  },
  // existing endpoints, re-exported for the onboarding modules
  settings: api.settings,
  providers: api.providers,
  createProvider: (input, o) => post('/api/providers', input, o),
  patchProvider: (id, input, o) => request('PATCH', `/api/providers/${enc(id)}`, input, o),
  testProvider: (id, o) => post(`/api/providers/${enc(id)}/test`, undefined, o),
  probeModels: (id, o) => post(`/api/providers/${enc(id)}/models`, undefined, o),
  setRole: (role, providerId, model, o) => put(`/api/settings/roles/${enc(role)}`, { provider_id: providerId, model, params: {} }, o),
  devices: (o) => get('/api/asr/devices', o),
  startSelfTest: (id, o) => post(`/api/asr/devices/${enc(id)}/selftest`, undefined, o),
  selfTest: (id, o) => get(`/api/asr/devices/${enc(id)}/selftest`, o),
  cancelSelfTest: (id, o) => request('DELETE', `/api/asr/devices/${enc(id)}/selftest`, undefined, o),
  applySelfTest: (id, model, precision, o) => post(`/api/asr/devices/${enc(id)}/selftest/apply`, { model, precision }, o),
};

/** The desktop shell's bridge (SPEC-v3 V1.4), or null in a browser. */
export function desktop() {
  return typeof window !== 'undefined' && window.lectureflow && typeof window.lectureflow === 'object' ? window.lectureflow : null;
}

/** Opens an external page: through the desktop shell, else a new tab. */
export function openExternal(url) {
  const d = desktop();
  if (d && typeof d.openExternal === 'function') {
    d.openExternal(url).catch(() => {});
    return;
  }
  window.open(url, '_blank', 'noopener');
}
