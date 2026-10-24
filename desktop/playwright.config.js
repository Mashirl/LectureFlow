// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// End-to-end tests of the shell (SPEC-v3 V12.1) with Playwright's Electron
// support. Two services:
//   fake  test/fixtures/fake-server.js (contract double; lifecycle cases)
//   go    the real service, built once by go.setup.js from ../cmd/lectureflow
'use strict';

const { defineConfig } = require('@playwright/test');

module.exports = defineConfig({
  testDir: './test/e2e',
  timeout: 90_000,
  expect: { timeout: 15_000 },
  workers: 1,
  fullyParallel: false,
  reporter: [['list']],
  outputDir: './test-results',
  projects: [
    // update.spec.js needs the service's update relay (internal/api): Go only.
    { name: 'fake', testMatch: /.*\.spec\.js$/, testIgnore: /update\.spec\.js$/, use: { serverKind: 'fake' } },
    { name: 'go-build', testMatch: /go\.setup\.js$/ },
    { name: 'go', dependencies: ['go-build'], testMatch: /(shell|update)\.spec\.js$/, use: { serverKind: 'go' } },
  ],
});
