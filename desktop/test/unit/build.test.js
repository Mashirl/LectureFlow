// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Packaging (SPEC-v3 V7, V9, V10): fuses, signing from the environment,
// pinned downloads, the LGPL check of ffmpeg, the size report.
'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const crypto = require('node:crypto');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');

const { signingFrom, dropEmptySigning } = require('../../scripts/lib/signing');
const { fetchPinned, sha256File, globFiles, cacheName } = require('../../scripts/lib/fetch');
const { buildSizeReport, renderMarkdown, groupTopLevel, walk, BUDGET_BYTES } = require('../../scripts/lib/sizereport');
const { checkConfigure, checkLicenseText, gplFree, gplComponents, targetKey, WORKER_FILES } = require('../../scripts/stage');

const DESKTOP = path.join(__dirname, '..', '..');
const REPO = path.join(DESKTOP, '..');

function loadConfig(env) {
  const saved = { ...process.env };
  for (const k of ['CSC_LINK', 'CSC_NAME', 'WIN_CSC_LINK', 'APPLE_API_KEY', 'APPLE_API_KEY_ID', 'APPLE_API_ISSUER', 'APPLE_ID', 'APPLE_APP_SPECIFIC_PASSWORD', 'APPLE_TEAM_ID', 'LECTUREFLOW_UPDATE_REPO', 'LECTUREFLOW_PUBLISHER_NAME', 'GH_TOKEN', 'GITHUB_TOKEN']) delete process.env[k];
  Object.assign(process.env, env);
  const p = require.resolve('../../electron-builder.config.js');
  delete require.cache[p];
  try {
    return require(p);
  } finally {
    for (const k of Object.keys(process.env)) if (!(k in saved)) delete process.env[k];
    Object.assign(process.env, saved);
    delete require.cache[p];
  }
}

test('electron-builder config: fuses, targets, per-user NSIS, no update feed by default', () => {
  const c = loadConfig({});
  assert.equal(c.appId, 'org.lectureflow.desktop');
  assert.deepEqual(c.electronFuses, {
    runAsNode: false,
    enableNodeOptionsEnvironmentVariable: false,
    enableNodeCliInspectArguments: false,
    enableCookieEncryption: true,
    enableEmbeddedAsarIntegrityValidation: true,
    onlyLoadAppFromAsar: true,
    loadBrowserProcessSpecificV8Snapshot: false,
    grantFileProtocolExtraPrivileges: true,
  });
  assert.equal(c.asar, true);
  assert.deepEqual(c.win.target.map((t) => t.target), ['nsis', 'portable']);
  assert.equal(c.nsis.oneClick, false);
  assert.equal(c.nsis.perMachine, false);
  assert.equal(c.nsis.allowToChangeInstallationDirectory, true);
  assert.equal(c.nsis.deleteAppDataOnUninstall, false);
  assert.deepEqual(c.mac.target.map((t) => [t.target, t.arch]), [['dmg', ['arm64']], ['zip', ['arm64']]]);
  assert.equal(c.publish, null, 'no LECTUREFLOW_UPDATE_REPO: explicitly no publish configuration');
  assert.equal(c.win.verifyUpdateCodeSignature, true);
  assert.equal(c.win.signtoolOptions, undefined);
  assert.deepEqual(c.extraResources, [{ from: 'build/resources', to: '.', filter: ['**/*'] }]);
  assert.ok(!c.files.some((f) => /test|scripts|\.cache|build\//.test(f)), 'tests and build scripts stay out of app.asar');
  assert.ok(fs.existsSync(path.join(DESKTOP, c.mac.entitlements)));
});

test('signing comes from the environment only; none = unsigned, no error', () => {
  assert.deepEqual(signingFrom({}), { macCert: false, macNotarize: false, winCert: false });
  assert.deepEqual(signingFrom({ CSC_LINK: '  ' }), { macCert: false, macNotarize: false, winCert: false }, 'empty secrets count as absent');
  assert.deepEqual(signingFrom({ WIN_CSC_LINK: 'x.pfx' }), { macCert: false, macNotarize: false, winCert: true });
  assert.equal(signingFrom({ CSC_LINK: 'x', APPLE_API_KEY: 'k', APPLE_API_KEY_ID: 'i' }).macNotarize, false, 'incomplete notarization credentials');
  assert.equal(signingFrom({ CSC_LINK: 'x', APPLE_API_KEY: 'k', APPLE_API_KEY_ID: 'i', APPLE_API_ISSUER: 's' }).macNotarize, true);
  assert.equal(signingFrom({ APPLE_ID: 'a', APPLE_APP_SPECIFIC_PASSWORD: 'p', APPLE_TEAM_ID: 't' }).macNotarize, false, 'no notarization without a certificate');
  // unset secrets arrive as empty strings: removed before electron-builder runs
  assert.deepEqual(dropEmptySigning({ CSC_LINK: '', CSC_KEY_PASSWORD: ' ', APPLE_API_KEY_ID: 'i', PATH: '' }), { APPLE_API_KEY_ID: 'i', PATH: '' });
  const unsigned = loadConfig({});
  assert.equal(unsigned.mac.identity, '-', 'ad-hoc signature: Apple silicon only runs signed code');
  assert.equal(unsigned.mac.notarize, false);
  const signed = loadConfig({ CSC_LINK: 'cert.p12', APPLE_ID: 'a', APPLE_APP_SPECIFIC_PASSWORD: 'p', APPLE_TEAM_ID: 't' });
  assert.equal(signed.mac.identity, undefined);
  assert.equal(signed.mac.notarize, true);
});

test('the main process never starts the crash reporter (no dumps that could hold the token)', () => {
  const src = path.join(DESKTOP, 'src');
  const files = [];
  const rec = (d) => fs.readdirSync(d, { withFileTypes: true }).forEach((e) => (e.isDirectory() ? rec(path.join(d, e.name)) : files.push(path.join(d, e.name))));
  rec(src);
  for (const f of files) assert.doesNotMatch(fs.readFileSync(f, 'utf8'), /crashReporter\s*\.\s*start|crashReporter\s*\.\s*setUploadToServer/, f);
});

test('third-party manifest: pinned HTTPS URLs and hashes, LGPL ffmpeg, binaries.json in step', () => {
  const m = JSON.parse(fs.readFileSync(path.join(DESKTOP, 'packaging', 'third-party.json'), 'utf8'));
  const pins = [m.ffmpeg['windows-x64'], m.ffmpeg['darwin-arm64'], m.uv['windows-x64'], m.uv['darwin-arm64'], ...m.uv.license_texts];
  for (const p of pins) {
    assert.match(p.url, /^https:\/\//, p.url);
    assert.match(p.sha256, /^[0-9a-f]{64}$/, p.url);
  }
  assert.deepEqual(checkConfigure(m.ffmpeg['darwin-arm64'].configure.join(' '), m.ffmpeg.forbidden_configure), []);
  assert.ok(m.ffmpeg['darwin-arm64'].configure.includes('--disable-gpl'));
  const bins = JSON.parse(fs.readFileSync(path.join(REPO, 'packaging', 'licenses', 'binaries.json'), 'utf8')).binaries;
  const text = JSON.stringify(bins);
  for (const p of pins.slice(0, 4)) assert.ok(text.includes(p.sha256), `binaries.json lacks ${p.sha256}`);
  assert.ok(bins.every((b) => !/to be pinned/.test(b.version)));
  assert.ok(text.includes(m.ffmpeg.version) && text.includes(m.uv.version));
});

test('ffmpeg: both builds from the pinned FFmpeg tarball, corresponding source and linked libraries declared, no GPL', () => {
  const m = JSON.parse(fs.readFileSync(path.join(DESKTOP, 'packaging', 'third-party.json'), 'utf8'));
  const bins = JSON.parse(fs.readFileSync(path.join(REPO, 'packaging', 'licenses', 'binaries.json'), 'utf8')).binaries;
  const win = bins.find((b) => b.name === 'ffmpeg / ffprobe (Windows build)');
  const mac = bins.find((b) => b.name === 'ffmpeg / ffprobe (macOS build)');
  const w = m.ffmpeg['windows-x64'];
  for (const [entry, pin] of [[win, w], [mac, m.ffmpeg['darwin-arm64']]]) {
    assert.deepEqual(entry.source_code, pin.source_code.map((s) => s.url), 'both manifests name the same source');
    for (const u of entry.source_code) assert.match(u, /^https:\/\//);
    assert.ok(Array.isArray(entry.bundled));
    assert.equal(entry.sha256, pin.sha256);
    assert.equal(entry.license, 'LGPL-2.1-or-later');
    assert.ok(entry.source_code.includes(pin.url));
  }
  // Windows: built from source, every input pinned by URL and SHA-256, nothing from BtbN.
  assert.equal(w.url, m.ffmpeg['darwin-arm64'].url, 'same FFmpeg source as macOS');
  assert.ok(!JSON.stringify(w).includes('BtbN') && !JSON.stringify(win).includes('BtbN'));
  assert.match(w.docker_image, /@sha256:[0-9a-f]{64}$/);
  for (const d of w.deps) {
    assert.match(d.url, /^https:\/\//, d.name);
    assert.match(d.sha256, /^[0-9a-f]{64}$/, d.name);
    assert.match(d.commit, /^[0-9a-f]{40}$/, d.name);
    assert.ok(win.source_code.includes(d.url), `${d.name} source listed`);
    assert.ok(win.bundled.some((l) => l.name.startsWith(d.name) && l.version.startsWith(d.version)), `${d.name} bundled`);
  }
  assert.deepEqual(checkConfigure(w.configure.join(' '), m.ffmpeg.forbidden_configure), []);
  for (const f of ['--disable-gpl', '--disable-nonfree', '--disable-version3', '--disable-autodetect', '--enable-shared']) assert.ok(w.configure.includes(f), f);
  for (const f of w.configure.filter((a) => /^--enable-lib/.test(a))) assert.ok(['--enable-libvpl', '--enable-libopenh264', '--enable-libdav1d'].includes(f), `unexpected external library ${f}`);
  assert.ok(Object.keys(w.toolchain).length >= 2);
  for (const v of Object.values(w.toolchain)) assert.ok(win.bundled.some((l) => l.version.includes(v)), `toolchain ${v} declared in bundled`);
  for (const l of win.bundled) {
    assert.ok(l.name && l.version && l.license && l.url, JSON.stringify(l));
    assert.match(l.url, /^https:\/\//, l.name);
  }
  assert.deepEqual(gplComponents(bins), [], 'no GPL component in the distributed ffmpeg (SPEC-v3 V7)');
  // macOS: system libraries plus dav1d (AV1 software decoder), pinned like the Windows deps.
  const md = m.ffmpeg['darwin-arm64'];
  assert.deepEqual(md.deps.map((d) => d.name), ['dav1d']);
  for (const d of md.deps) {
    assert.deepEqual(d, w.deps.find((x) => x.name === d.name), `${d.name}: same pin as Windows`);
    assert.ok(mac.source_code.includes(d.url), `${d.name} source listed`);
    assert.ok(mac.bundled.some((l) => l.name.startsWith(d.name) && l.version.startsWith(d.version)), `${d.name} bundled`);
  }
  assert.equal(mac.bundled.length, md.deps.length, 'macOS: nothing else bundled');
  for (const f of md.configure.filter((a) => /^--enable-lib/.test(a))) assert.ok(['--enable-libdav1d'].includes(f), `unexpected external library ${f}`);
  for (const t of md.build_tools) assert.match(t.sha256, /^[0-9a-f]{64}$/, t.name);
});

test('GPL check of ffmpeg components and of ffmpeg -L', () => {
  for (const ok of ['LGPL-2.1-or-later', 'MIT', 'BSD-2-Clause', 'Zlib', 'FTL OR GPL-2.0-or-later', 'Apache-2.0 OR GPL-2.0-or-later', 'GPL-3.0-or-later WITH GCC-exception-3.1', 'LGPL-3.0-or-later OR GPL-2.0-or-later', 'ZPL-2.1 AND MIT']) assert.ok(gplFree(ok), ok);
  for (const bad of ['GPL-2.0-or-later', 'GPL-2.0-only', 'LGPL-2.0-or-later AND GPL-2.0-only AND MIT', 'MIT AND GPL-3.0-or-later', 'AGPL-3.0-only', 'GPL-3.0-or-later WITH Bison-exception-2.2']) assert.ok(!gplFree(bad), bad);
  assert.deepEqual(gplComponents([{ name: 'ffmpeg / ffprobe (x)', license: 'LGPL-2.1-or-later', bundled: [{ name: 'FFTW', license: 'GPL-2.0-or-later' }, { name: 'zlib', license: 'Zlib' }] }, { name: 'uv', license: 'GPL-2.0-only' }]),
    ['ffmpeg / ffprobe (x) / FFTW [GPL-2.0-or-later]']);
  const lgpl = 'ffmpeg is free software; you can redistribute it and/or\nmodify it under the terms of the GNU Lesser General Public\nLicense as published by the Free Software Foundation; either\nversion 2.1 of the License, or (at your option) any later version.';
  assert.ok(checkLicenseText(lgpl));
  assert.ok(!checkLicenseText('ffmpeg is free software; you can redistribute it and/or modify it under the terms of the GNU General Public License as published by the Free Software Foundation; either version 2 of the License'));
  assert.ok(!checkLicenseText(lgpl.replace('2.1', '3')), 'version3 builds are not what is declared');
});

test('ffmpeg configure check: GPL and non-free flags fail the build', () => {
  const forbidden = ['--enable-gpl', '--enable-nonfree', '--enable-libx264', '--enable-libx265'];
  assert.deepEqual(checkConfigure('--enable-version3 --enable-shared --disable-libx264', forbidden), []);
  assert.deepEqual(checkConfigure('--prefix=/x --enable-gpl --enable-libx264', forbidden), ['--enable-gpl', '--enable-libx264']);
  assert.deepEqual(checkConfigure('--enable-nonfree', forbidden), ['--enable-nonfree']);
  assert.equal(targetKey('win32'), 'windows-x64');
  assert.equal(targetKey('darwin'), 'darwin-arm64');
  assert.throws(() => targetKey('linux'), /unsupported/);
  for (const f of WORKER_FILES) assert.ok(fs.existsSync(path.join(REPO, 'worker', f)), f);
  assert.ok(WORKER_FILES.every((f) => !/^test_|conftest/.test(f)));
});

test('pinned downloads: verified before use, cached by hash, mismatches rejected', async () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'lf-fetch-'));
  try {
    const body = Buffer.from('pinned payload');
    const sha = crypto.createHash('sha256').update(body).digest('hex');
    let calls = 0;
    const fakeFetch = async () => {
      calls++;
      return new Response(body);
    };
    const quiet = { fetchImpl: fakeFetch, log: () => {} };
    const f = await fetchPinned({ url: 'https://example.com/a/tool.zip', sha256: sha }, dir, quiet);
    assert.equal(sha256File(f), sha);
    assert.equal(path.basename(f), `${sha.slice(0, 12)}-tool.zip`);
    await fetchPinned({ url: 'https://example.com/a/tool.zip', sha256: sha }, dir, quiet);
    assert.equal(calls, 1, 'second call served from the cache');
    await assert.rejects(fetchPinned({ url: 'https://example.com/b.zip', sha256: 'a'.repeat(64) }, dir, quiet), /SHA-256 mismatch/);
    assert.deepEqual(fs.readdirSync(dir).filter((n) => n.includes('b.zip')), [], 'nothing kept from a bad download');
    await assert.rejects(fetchPinned({ url: 'http://example.com/c.zip', sha256: sha }, dir, quiet), /non-HTTPS/);
    assert.equal(cacheName('https://x/y/ffmpeg%20n9.zip?x=1'), 'ffmpeg_n9.zip');
    fs.mkdirSync(path.join(dir, 'bin'));
    for (const n of ['ffmpeg.exe', 'avcodec-63.dll', 'avutil-61.dll', 'readme.txt']) fs.writeFileSync(path.join(dir, 'bin', n), 'x');
    assert.deepEqual(globFiles(dir, 'bin/*.dll').map((p) => path.basename(p)).sort(), ['avcodec-63.dll', 'avutil-61.dll']);
    assert.deepEqual(globFiles(dir, 'bin/ffmpeg.exe').map((p) => path.basename(p)), ['ffmpeg.exe']);
    assert.deepEqual(globFiles(dir, 'bin/missing.exe'), []);
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

test('size report: budget, resources, ten largest files, Markdown', () => {
  const MB = 1024 * 1024;
  const report = buildSizeReport({
    version: '3.0.0',
    platform: 'windows-x64',
    artifacts: [{ name: 'Setup.exe', bytes: 180 * MB }, { name: 'big.exe', bytes: 260 * MB }],
    resourceFiles: [
      { path: 'bin/avcodec-63.dll', bytes: 87 * MB }, { path: 'bin/uv.exe', bytes: 31 * MB },
      { path: 'worker/asr.py', bytes: 100 }, { path: 'LICENSE', bytes: 34000 },
    ],
    asarBytes: 1.2 * MB,
    unpackedFiles: Array.from({ length: 15 }, (_, i) => ({ path: `f${i}`, bytes: i * MB })),
    updates: 'Automatic updates: off (LECTUREFLOW_UPDATE_REPO is not set)',
  });
  assert.equal(report.budgetBytes, BUDGET_BYTES);
  assert.equal(report.overBudget, true);
  assert.deepEqual(report.installers.map((a) => a.overBudget), [false, true]);
  assert.deepEqual(report.resources.map((g) => g.name), ['bin/', 'LICENSE', 'worker/']);
  assert.equal(report.bin[0].path, 'bin/avcodec-63.dll');
  assert.equal(report.largest.length, 10);
  assert.equal(report.largest[0].path, 'f14');
  const md = renderMarkdown(report);
  assert.match(md, /\| Setup\.exe \| 180\.0 MB \| yes \|/);
  assert.match(md, /\| big\.exe \| 260\.0 MB \| no \|/);
  assert.match(md, /Over budget\./);
  assert.match(md, /Ten largest files/);
  assert.match(md, /Automatic updates: off \(LECTUREFLOW_UPDATE_REPO is not set\)\./);
  assert.deepEqual(groupTopLevel([{ path: 'a/b', bytes: 1 }, { path: 'a/c', bytes: 2 }, { path: 'x', bytes: 5 }]), [
    { name: 'x', bytes: 5, files: 1 }, { name: 'a/', bytes: 3, files: 2 },
  ]);
  assert.ok(walk(path.join(DESKTOP, 'locales')).some((f) => f.path === 'en.json'));
});

test('update feed: none without LECTUREFLOW_UPDATE_REPO, the named repository with it, malformed fails', () => {
  for (const env of [{}, { LECTUREFLOW_UPDATE_REPO: '' }, { GH_TOKEN: 'x' }]) {
    assert.equal(loadConfig(env).publish, null, JSON.stringify(env));
  }
  const c = loadConfig({ LECTUREFLOW_UPDATE_REPO: 'example-owner/example.repo_1' });
  assert.deepEqual(c.publish, [{ provider: 'github', owner: 'example-owner', repo: 'example.repo_1', releaseType: 'draft' }]);
  assert.equal(c.win.signtoolOptions, undefined, 'unsigned: no publisher name');
  const bad = ['lectureflow', 'a/b/c', ' owner/name', 'owner/name ', 'https://github.com/o/r', 'o/r.git', '-o/r', 'o-/r', 'o--x/r', 'o/..', 'o/', '/r', 'ow ner/r', 'o/r\n'];
  for (const v of bad) assert.throws(() => loadConfig({ LECTUREFLOW_UPDATE_REPO: v }), /LECTUREFLOW_UPDATE_REPO must be/, JSON.stringify(v));
});

test('update feed: Windows publisher name only for signed builds', () => {
  const signed = loadConfig({ LECTUREFLOW_UPDATE_REPO: 'o/r', WIN_CSC_LINK: 'cert.pfx', LECTUREFLOW_PUBLISHER_NAME: ' Example Publisher Ltd ' });
  assert.deepEqual(signed.win.signtoolOptions, { publisherName: 'Example Publisher Ltd' });
  assert.equal(signed.win.verifyUpdateCodeSignature, true);
  const derived = loadConfig({ LECTUREFLOW_UPDATE_REPO: 'o/r', WIN_CSC_LINK: 'cert.pfx' });
  assert.equal(derived.win.signtoolOptions, undefined, 'electron-builder takes the common name of the certificate');
  const unsigned = loadConfig({ LECTUREFLOW_UPDATE_REPO: 'o/r', LECTUREFLOW_PUBLISHER_NAME: 'Example Publisher Ltd' });
  assert.equal(unsigned.win.signtoolOptions, undefined, 'ignored without a certificate');
  assert.throws(() => loadConfig({ LECTUREFLOW_UPDATE_REPO: 'o/r', WIN_CSC_LINK: 'cert.pfx', LECTUREFLOW_PUBLISHER_NAME: 'a\nb' }), /LECTUREFLOW_PUBLISHER_NAME/);
});

test('update feed check of a finished build', () => {
  const { checkUpdateFeed, updateBuildConfig } = require('../../scripts/lib/updaterepo');
  assert.match(updateBuildConfig({}).summary, /Automatic updates: off .*LECTUREFLOW_UPDATE_REPO is not set/);
  assert.match(updateBuildConfig({ LECTUREFLOW_UPDATE_REPO: 'o/r' }).summary, /Automatic updates: on .*o\/r/);
  assert.deepEqual(checkUpdateFeed({ repo: null, appUpdateYml: null, distFiles: ['LectureFlow-Setup-3.0.0-x64.exe', 'size-report.md'], platform: 'win32' }), []);
  assert.equal(checkUpdateFeed({ repo: null, appUpdateYml: 'provider: github\n', distFiles: [], platform: 'win32' }).length, 1);
  assert.equal(checkUpdateFeed({ repo: null, appUpdateYml: null, distFiles: ['latest.yml'], platform: 'win32' }).length, 1);
  const yml = 'owner: o\nrepo: r\nprovider: github\nreleaseType: draft\nupdaterCacheDirName: lectureflow-desktop-updater\n';
  const repo = { owner: 'o', repo: 'r' };
  assert.deepEqual(checkUpdateFeed({ repo, appUpdateYml: yml, distFiles: ['latest.yml'], platform: 'win32' }), []);
  assert.equal(checkUpdateFeed({ repo, appUpdateYml: null, distFiles: [], platform: 'win32' }).length, 1);
  assert.match(checkUpdateFeed({ repo: { owner: 'o', repo: 'x' }, appUpdateYml: yml, platform: 'win32' })[0], /expected github:o\/x/);
  assert.match(checkUpdateFeed({ repo, appUpdateYml: yml, platform: 'win32', winCert: true })[0], /publisherName/);
  assert.deepEqual(checkUpdateFeed({ repo, appUpdateYml: `${yml}publisherName:\n  - Example Publisher Ltd\n`, platform: 'win32', winCert: true }), []);
  assert.deepEqual(checkUpdateFeed({ repo, appUpdateYml: yml, platform: 'darwin', winCert: true }), [], 'publisherName is Windows only');
});

test('package.json names no repository that electron-builder could pick up', () => {
  const pkg = JSON.parse(fs.readFileSync(path.join(DESKTOP, 'package.json'), 'utf8'));
  assert.equal(pkg.repository, undefined);
  assert.equal(pkg.homepage, undefined);
  assert.ok(!fs.existsSync(path.join(DESKTOP, 'dev-app-update.yml')));
});
