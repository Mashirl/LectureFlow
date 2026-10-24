# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
# Task runner for Windows (no make). Usage: .\make.ps1 build|test|run|realgpu|audit
param([Parameter(Position=0)][string]$Task = "test")
$ErrorActionPreference = "Stop"
Set-Location $PSScriptRoot
$py = Join-Path $PSScriptRoot ".venv\Scripts\python.exe"

switch ($Task) {
  "build" {
    go build -o lectureflow.exe ./cmd/lectureflow
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
  }
  "test" {
    go vet ./...
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
    go test -p 4 ./...
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
    if (Get-ChildItem worker -Filter "test_*.py" -ErrorAction SilentlyContinue) {
      & $py -m pytest -q worker
      if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
    }
    # Electron shell unit tests (SPEC-v3 V12.1) with the project's portable
    # Node (tools/node, not in git); skipped when Node or the packages are
    # missing. The end-to-end suite runs separately: cd desktop; npm run test:e2e
    $node = Join-Path $PSScriptRoot "tools\node"
    if ((Test-Path (Join-Path $node "npm.cmd")) -and (Test-Path "desktop\node_modules")) {
      $env:PATH = "$node;$env:PATH"
      Push-Location desktop
      & (Join-Path $node "npm.cmd") test --silent
      $code = $LASTEXITCODE
      Pop-Location
      if ($code -ne 0) { exit $code }
    } else {
      Write-Host "desktop: skipped (no tools\node or desktop\node_modules)"
    }
  }
  "realgpu" {
    # Opt-in tests that use the real GPU (CLAUDE.md revision 2). Not part of "test".
    go test -p 1 -tags realgpu ./...
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
    & $py -m pytest -q -m realgpu worker
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
  }
  "audit" {
    # License audit (SPEC-v3 V7): refreshes THIRD_PARTY_NOTICES.md and checks
    # SPDX headers; exits non-zero when a license is incompatible or unknown.
    go run ./scripts/licaudit
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
  }
  "run" {
    go build -o lectureflow.exe ./cmd/lectureflow
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
    if (-not $env:DEEPSEEK_API_KEY) {
      # Pick up a key set with setx (user scope) without restarting the shell.
      $k = [Environment]::GetEnvironmentVariable("DEEPSEEK_API_KEY", "User")
      if ($k) { $env:DEEPSEEK_API_KEY = $k }
    }
    .\lectureflow.exe -config config.toml
  }
  default { Write-Error "unknown task $Task (build|test|run|realgpu|audit)"; exit 2 }
}
