// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Builds the real service once for the "go" project:
//   go build -ldflags "-X lectureflow/internal/selfupdate.builtinPublicKey=" -o desktop/.e2e-tmp/bin/lectureflow(.exe) ./cmd/lectureflow
// without the maintainer's update key (built in since 2026-10-27): with it
// the shell takes the in-app update and never loads electron-updater, and
// update.spec.js tests the electron-updater relay (SPEC-v3 V8), which only a
// build without the key uses. The key's own tests are in
// internal/selfupdate; the choice of path is rollback.spec.js.
'use strict';

const { execFileSync } = require('node:child_process');
const fs = require('node:fs');
const path = require('node:path');
const { test } = require('@playwright/test');
const { GO_BIN, REPO } = require('./fixtures');

test('build the Go service', async () => {
  test.setTimeout(600_000);
  fs.mkdirSync(path.dirname(GO_BIN), { recursive: true });
  execFileSync('go', ['build', '-ldflags', '-X lectureflow/internal/selfupdate.builtinPublicKey=', '-o', GO_BIN, './cmd/lectureflow'],
    { cwd: REPO, stdio: 'inherit' });
});
