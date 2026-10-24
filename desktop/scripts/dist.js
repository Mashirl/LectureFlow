// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Builds the installers (SPEC-v3 V10):
//   1. icon (build/icon.png, the placeholder mark until real artwork exists)
//   2. stage the resources (scripts/stage.js)
//   3. licence audit (go run ./scripts/licaudit -check): a violation fails the build
//   4. electron-builder: Windows NSIS + portable, or macOS arm64 dmg (+ zip)
//   5. update feed check: app-update.yml only when LECTUREFLOW_UPDATE_REPO is set
//      (scripts/lib/updaterepo.js); a mismatch fails the build
//   6. size report: dist/size-report.{md,json} (over 250 MB is reported, not fatal)
//
// Usage: node scripts/dist.js [--skip-stage] [--skip-audit] [--publish never|always] [--out DIR]
'use strict';

const { execFileSync } = require('node:child_process');
const fs = require('node:fs');
const path = require('node:path');
const { iconPng } = require('../src/main/lib/pngicon');
const { stage } = require('./stage');
const { signingFrom, dropEmptySigning } = require('./lib/signing');
const { updateBuildConfig, checkUpdateFeed } = require('./lib/updaterepo');
const { walk, buildSizeReport, renderMarkdown, mb } = require('./lib/sizereport');

const DESKTOP = path.resolve(__dirname, '..');
const REPO = path.resolve(DESKTOP, '..');
// --out DIR: build into another directory (e.g. dist-next while dist/ is in use).
const OUT_ARG = process.argv.indexOf('--out') > 0 ? process.argv[process.argv.indexOf('--out') + 1] : 'dist';
if (!/^[A-Za-z0-9._-]+$/.test(OUT_ARG) || OUT_ARG === '.' || OUT_ARG === '..') throw new Error('--out must be a directory name inside desktop/');
const DIST = path.join(DESKTOP, OUT_ARG);

function arg(name) {
  const i = process.argv.indexOf(name);
  return i > 0 ? process.argv[i + 1] : undefined;
}

function artifactsOf(platform) {
  if (!fs.existsSync(DIST)) return [];
  const re = platform === 'win32' ? /\.exe$/i : /\.(dmg|zip)$/i;
  return fs.readdirSync(DIST)
    .filter((f) => re.test(f) && !/^__uninstaller/i.test(f))
    .map((f) => ({ name: f, bytes: fs.statSync(path.join(DIST, f)).size }));
}

function unpackedDir(platform) {
  if (platform === 'win32') return path.join(DIST, 'win-unpacked');
  const mac = path.join(DIST, 'mac-arm64', 'LectureFlow.app');
  return mac;
}

function resourcesOf(platform) {
  const u = unpackedDir(platform);
  return platform === 'win32' ? path.join(u, 'resources') : path.join(u, 'Contents', 'Resources');
}

/** ffmpegChecks() → the LGPL checks stage.js ran (build/stage-info.json), one line. */
function ffmpegChecks() {
  try {
    const info = JSON.parse(fs.readFileSync(path.join(DESKTOP, 'build', 'stage-info.json'), 'utf8'));
    const c = info.ffmpeg && info.ffmpeg.license_checks;
    return Array.isArray(c) && c.length ? `ffmpeg ${info.ffmpeg.version} LGPL checks passed: ${c.join('; ')}` : '';
  } catch {
    return '';
  }
}

async function main() {
  const platform = process.platform;
  if (platform !== 'win32' && platform !== 'darwin') throw new Error('installers are built on Windows or macOS');
  const pkg = JSON.parse(fs.readFileSync(path.join(DESKTOP, 'package.json'), 'utf8'));
  // Before anything slow: a malformed LECTUREFLOW_UPDATE_REPO fails here.
  const updates = updateBuildConfig(process.env, { winCert: signingFrom(process.env).winCert });
  console.log(updates.summary);
  fs.mkdirSync(path.join(DESKTOP, 'build'), { recursive: true });
  fs.writeFileSync(path.join(DESKTOP, 'build', 'icon.png'), iconPng(512));

  if (!process.argv.includes('--skip-stage')) await stage({ platform, version: pkg.version });
  if (!process.argv.includes('--skip-audit')) {
    execFileSync('go', ['run', './scripts/licaudit', '-check'], { cwd: REPO, stdio: 'inherit' });
  }

  dropEmptySigning(process.env);
  const signing = signingFrom(process.env);
  console.log(platform === 'win32'
    ? `Windows code signing: ${signing.winCert ? 'certificate from CSC_LINK / WIN_CSC_LINK' : 'none (unsigned build, see docs/install.md)'}`
    : `macOS signing: ${signing.macCert ? 'Developer ID' : 'ad-hoc (unsigned build, see docs/install.md)'}; notarization: ${signing.macNotarize ? 'yes' : 'no'}`);
  if (platform === 'darwin' && !signing.macCert) process.env.CSC_IDENTITY_AUTO_DISCOVERY = 'false';

  for (const f of artifactsOf(platform)) fs.rmSync(path.join(DIST, f.name), { force: true });
  if (fs.existsSync(DIST)) for (const f of fs.readdirSync(DIST)) if (/^latest.*\.ya?ml$/i.test(f)) fs.rmSync(path.join(DIST, f), { force: true });
  const builder = require('electron-builder');
  const config = require('../electron-builder.config.js');
  const targets = platform === 'win32' ? builder.Platform.WINDOWS.createTarget() : builder.Platform.MAC.createTarget();
  const buildConfig = { ...config, directories: { ...config.directories, output: OUT_ARG } };
  await builder.build({ targets, config: buildConfig, publish: arg('--publish') || 'never', projectDir: DESKTOP });

  const res = resourcesOf(platform);
  const yml = path.join(res, 'app-update.yml');
  const problems = checkUpdateFeed({
    repo: updates.repo,
    appUpdateYml: fs.existsSync(yml) ? fs.readFileSync(yml, 'utf8') : null,
    distFiles: fs.readdirSync(DIST),
    platform,
    winCert: signing.winCert,
  });
  if (problems.length) throw new Error(`update feed check failed:\n  ${problems.join('\n  ')}`);
  console.log(updates.summary);
  const resourceFiles = walk(res).filter((f) => f.path !== 'app.asar');
  const asar = path.join(res, 'app.asar');
  const report = buildSizeReport({
    version: pkg.version,
    platform: platform === 'win32' ? 'windows-x64' : 'darwin-arm64',
    artifacts: artifactsOf(platform),
    resourceFiles,
    asarBytes: fs.existsSync(asar) ? fs.statSync(asar).size : 0,
    unpackedFiles: walk(unpackedDir(platform)),
    updates: updates.summary,
    ffmpegChecks: ffmpegChecks(),
  });
  fs.writeFileSync(path.join(DIST, 'size-report.json'), `${JSON.stringify(report, null, 2)}\n`);
  fs.writeFileSync(path.join(DIST, 'size-report.md'), renderMarkdown(report));
  for (const a of report.installers) console.log(`${a.name}: ${mb(a.bytes)}${a.overBudget ? ' (over the 250 MB budget)' : ''}`);
  console.log(`size report: ${path.join(DIST, 'size-report.md')}`);
}

main().catch((err) => {
  console.error(`dist failed: ${err && err.stack ? err.stack : err}`);
  process.exit(1);
});
