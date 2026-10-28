// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Provider rules shared by the settings page and onboarding: the limit
// ranges and the DeepSeek preset's limits (perf-llm), and whether a
// base_url edit moves a provider to another host (sec2-app L2: the back end
// then clears the stored key).

// Ranges of internal/settings (MaxConcurrencyLimit, MaxRPM); rpm 0 = no
// per-minute limit.
export const MAX_CONCURRENCY = 512;
export const MAX_RPM = 10000;

// DEEPSEEK_LIMITS are the DeepSeek preset's limits (packaging/components
// manifest, config.example.toml; web/perf_llm_test.go keeps them in step):
// DeepSeek limits concurrent requests per account and has no per-minute
// limit.
export const DEEPSEEK_LIMITS = Object.freeze({ max_concurrency: 64, rpm: 0 });

// OLD_LIMITS are the conservative presets of earlier versions (4 / 60, then
// 8 / 300). A DeepSeek provider at or below them is offered the new ones.
export const OLD_LIMITS = Object.freeze({ max_concurrency: 8, rpm: 300 });

function parse(u) {
  try {
    return new URL(String(u || '').trim());
  } catch {
    return null;
  }
}

function originOf(u) {
  const x = parse(u);
  if (!x || !x.host) return 'raw:' + String(u || '').trim();
  const scheme = x.protocol.replace(/:$/, '').toLowerCase();
  const port = x.port || (scheme === 'https' ? '443' : scheme === 'http' ? '80' : '');
  return `${scheme}://${x.hostname.toLowerCase()}:${port}`;
}

// sameOrigin mirrors settings.SameOrigin: scheme, host and port.
export function sameOrigin(a, b) {
  return originOf(a) === originOf(b);
}

// isDeepSeekAPI: the provider talks to DeepSeek's own API (a relay may have
// limits of its own, so only the official host counts).
export function isDeepSeekAPI(p) {
  const x = p && parse(p.base_url);
  return !!x && x.hostname.toLowerCase() === 'api.deepseek.com';
}

// hasOldLimits: a DeepSeek provider still at (or below) the old
// conservative limits; the settings page suggests the preset's limits (it
// never changes them by itself).
export function hasOldLimits(p) {
  if (!isDeepSeekAPI(p)) return false;
  const rpm = Number(p.rpm) || 0;
  return Number(p.max_concurrency) <= OLD_LIMITS.max_concurrency || (rpm > 0 && rpm <= OLD_LIMITS.rpm);
}

// The provider's requests go to base_url — trailing slashes removed, as
// internal/llm does — followed by the type's path (/chat/completions,
// /v1/messages). The dialog's hint names that address as typed (3.2.0 L18:
// a literal "{base_url}" read as a placeholder left unfilled).
// endpointParts(text, mark, base): text is the hint with mark where
// {base_url} goes; returns [before, address, after], the address being base
// (or "https://…" while the field is empty) with the path the text writes
// right after it (a full stop that ends the sentence is not part of it);
// [text] when the text has no mark.
export function endpointParts(text, mark, base) {
  const at = text.indexOf(mark);
  if (at < 0) return [text];
  const after = text.slice(at + mark.length);
  const path = /^[\w/-]*(?:\.[\w-]+)*/.exec(after)[0];
  const b = String(base || '').trim().replace(/\/+$/, '') || 'https://…';
  return [text.slice(0, at), b + path, after.slice(path.length)];
}
