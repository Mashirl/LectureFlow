# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
# Task runner for Windows (no make). Usage: .\make.ps1 build|test|run|realgpu|audit|agent|realssh
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
    # Browser extension (SPEC-ext): unit tests with the fixture replay, then
    # the profile and fixture check of CONTRIBUTING-profiles.md and its own
    # tests; same Node, skipped the same way. The end-to-end suite runs
    # separately: cd extension; npm run test:e2e
    if ((Test-Path (Join-Path $node "npm.cmd")) -and (Test-Path "extension\node_modules")) {
      $env:PATH = "$node;$env:PATH"
      & (Join-Path $node "npm.cmd") test --prefix extension --silent
      if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
      & (Join-Path $node "node.exe") scripts/profilecheck/check.mjs
      if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
      & (Join-Path $node "node.exe") --test "scripts/profilecheck/*.test.mjs"
      if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
    } else {
      Write-Host "extension: skipped (no tools\node or extension\node_modules)"
    }
  }
  "realgpu" {
    # Opt-in tests that use the real GPU (CLAUDE.md revision 2). Not part of "test".
    go test -p 1 -tags realgpu ./...
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
    & $py -m pytest -q -m realgpu worker
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
  }
  "agent" {
    # lf-agent for the SSH inference servers (ssh-mvp): one static build per
    # remote platform into tools\lf-agent\<platform>\lf-agent (git-ignored;
    # the service looks there in development, resources\agent when packaged),
    # versioned like the application; kit.json lists their SHA-256.
    $ver = (Get-Content desktop\package.json -Raw | ConvertFrom-Json).version
    $env:CGO_ENABLED = "0"
    $kit = [ordered]@{ version = $ver; builds = [ordered]@{} }
    foreach ($t in @(@("linux-x64", "linux", "amd64"), @("darwin-arm64", "darwin", "arm64"), @("windows-x64", "windows", "amd64"))) {
      $out = "tools\lf-agent\$($t[0])\lf-agent"
      $env:GOOS = $t[1]; $env:GOARCH = $t[2]
      go build -trimpath -ldflags "-s -w -X lectureflow/internal/agent.Version=$ver" -o $out ./cmd/lf-agent
      if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
      $kit.builds[$t[0]] = (Get-FileHash -Algorithm SHA256 $out).Hash.ToLower()
    }
    Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED
    $kit | ConvertTo-Json | Set-Content -Encoding utf8 tools\lf-agent\kit.json
  }
  "realssh" {
    # The real SSH test against the server named by LF_SSH_TEST_* (opt-in).
    go test -p 1 -count=1 -tags realssh -run RealSSH -v ./internal/sshremote/
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
  default { Write-Error "unknown task $Task (build|test|run|realgpu|audit|agent|realssh)"; exit 2 }
}
