// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Size report of a desktop build (SPEC-v3 V10, appendix F): installer
// sizes against the 250 MB budget, the bundled resources, app.asar and the
// ten largest files of the unpacked app. Over budget is reported, never a
// reason to fail the build or to drop features.
'use strict';

const fs = require('node:fs');
const path = require('node:path');

const BUDGET_BYTES = 250 * 1024 * 1024;

/** walk(dir) → [{ path (relative, "/"), bytes }] of every file below dir. */
function walk(dir) {
  const out = [];
  const rec = (d) => {
    for (const e of fs.readdirSync(d, { withFileTypes: true })) {
      const p = path.join(d, e.name);
      if (e.isDirectory()) rec(p);
      else if (e.isFile()) out.push({ path: path.relative(dir, p).split(path.sep).join('/'), bytes: fs.statSync(p).size });
    }
  };
  if (fs.existsSync(dir)) rec(dir);
  return out;
}

/** groupTopLevel(files) → [{ name, bytes, files }] by first path segment, largest first. */
function groupTopLevel(files) {
  const m = new Map();
  for (const f of files) {
    const top = f.path.includes('/') ? `${f.path.split('/')[0]}/` : f.path;
    const g = m.get(top) || { name: top, bytes: 0, files: 0 };
    g.bytes += f.bytes;
    g.files += 1;
    m.set(top, g);
  }
  return [...m.values()].sort((a, b) => b.bytes - a.bytes || a.name.localeCompare(b.name));
}

/**
 * buildSizeReport({ version, platform, artifacts: [{ name, bytes }],
 *   resourceFiles, binFiles, asarBytes, unpackedFiles, budgetBytes, updates })
 * updates: one line on the update feed of the build (scripts/lib/updaterepo.js).
 * Pure: the caller lists the files (walk()).
 */
function buildSizeReport({ version, platform, artifacts, resourceFiles = [], asarBytes = 0, unpackedFiles = [], budgetBytes = BUDGET_BYTES, updates = '', ffmpegChecks = '' }) {
  const installers = artifacts.map((a) => ({ ...a, overBudget: a.bytes > budgetBytes }));
  const largest = [...unpackedFiles].sort((a, b) => b.bytes - a.bytes || a.path.localeCompare(b.path)).slice(0, 10);
  const bin = resourceFiles.filter((f) => f.path.startsWith('bin/')).sort((a, b) => b.bytes - a.bytes);
  return {
    version,
    platform,
    budgetBytes,
    overBudget: installers.some((a) => a.overBudget),
    installers,
    asarBytes,
    resources: groupTopLevel(resourceFiles),
    bin,
    unpackedBytes: unpackedFiles.reduce((n, f) => n + f.bytes, 0),
    largest,
    updates,
    ffmpegChecks,
  };
}

function mb(bytes) {
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

/** renderMarkdown(report) → the report as Markdown. */
function renderMarkdown(r) {
  const lines = [];
  lines.push(`# LectureFlow ${r.version} size report (${r.platform})`, '');
  lines.push(`Budget per installer: ${mb(r.budgetBytes)} (SPEC-v3 V10, without components).`, '');
  lines.push('| Artifact | Size | Within budget |', '|---|---:|---|');
  for (const a of r.installers) lines.push(`| ${a.name} | ${mb(a.bytes)} | ${a.overBudget ? 'no' : 'yes'} |`);
  lines.push('');
  if (r.overBudget) {
    lines.push('**Over budget.** Built anyway (appendix F); the largest items are listed below.', '');
  }
  lines.push(`Unpacked app: ${mb(r.unpackedBytes)}; app.asar: ${mb(r.asarBytes)}.`, '');
  if (r.updates) lines.push(`${r.updates}.`, '');
  if (r.ffmpegChecks) lines.push(`${r.ffmpegChecks}.`, '');
  lines.push('## Bundled resources', '', '| Entry | Size | Files |', '|---|---:|---:|');
  for (const g of r.resources) lines.push(`| ${g.name} | ${mb(g.bytes)} | ${g.files} |`);
  lines.push('');
  if (r.bin.length) {
    lines.push('### resources/bin', '', '| File | Size |', '|---|---:|');
    for (const f of r.bin) lines.push(`| ${f.path} | ${mb(f.bytes)} |`);
    lines.push('');
  }
  lines.push('## Ten largest files of the unpacked app', '', '| File | Size |', '|---|---:|');
  for (const f of r.largest) lines.push(`| ${f.path} | ${mb(f.bytes)} |`);
  lines.push('');
  return lines.join('\n');
}

module.exports = { BUDGET_BYTES, walk, groupTopLevel, buildSizeReport, renderMarkdown, mb };
