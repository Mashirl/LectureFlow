# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
#
# Regenerates packaging/components/locks/*.txt from packaging/components/requirements/*.in
# with uv pip compile --generate-hashes (one lock per component and platform; macOS locks
# are cross-compiled with --python-platform). Then run
#   go run ./packaging/components/gen
# to refresh the hashes and sizes in components.json.
#
# Usage (repository root):
#   powershell -NoProfile -ExecutionPolicy Bypass -File packaging/components/refresh-locks.ps1 [-Uv tools\uv\uv.exe]
param(
    [string]$Uv = "tools\uv\uv.exe",
    [string]$PythonVersion = "3.12"
)
$ErrorActionPreference = "Stop"
$root = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
Set-Location $root
if (-not (Test-Path $Uv)) {
    throw "uv not found at $Uv (see packaging/components/README.md: download the pinned uv release into tools/uv/)"
}
$req = "packaging/components/requirements"
$out = "packaging/components/locks"
New-Item -ItemType Directory -Force $out | Out-Null

# A separate cache keeps the user's uv cache untouched.
$env:UV_CACHE_DIR = Join-Path $root "tools/uv/cache"
# uv's default macOS deployment target for --python-platform aarch64-apple-darwin is 13.0;
# pinned so the macOS locks do not change with uv versions.
$env:MACOSX_DEPLOYMENT_TARGET = "13.0"

$platforms = @{
    "windows-x64"  = "x86_64-pc-windows-msvc"
    "darwin-arm64" = "aarch64-apple-darwin"
    # linux-x64 is only a target of the SSH inference servers (ssh-mvp; glibc 2.28+)
    "linux-x64"    = "x86_64-manylinux_2_28"
}
# component -> platforms (see manifest.src.json "gen.locks")
$locks = [ordered]@{
    "python"       = @("windows-x64", "darwin-arm64", "linux-x64")
    "asr-cuda"     = @("windows-x64", "linux-x64")
    "asr-openvino" = @("windows-x64")
    "asr-mlx"      = @("darwin-arm64")
}

foreach ($comp in $locks.Keys) {
    foreach ($plat in $locks[$comp]) {
        $uvArgs = @(
            "pip", "compile", "--no-config", "$req/$comp.in",
            "--generate-hashes",
            "--python-version", $PythonVersion,
            "--python-platform", $platforms[$plat],
            "--only-binary", ":all:",
            "--custom-compile-command", "powershell -File packaging/components/refresh-locks.ps1",
            "-o", "$out/$comp-$plat.txt"
        )
        $constraints = "$req/$comp.constraints.txt"
        if (Test-Path $constraints) {
            $uvArgs += @("-c", $constraints)
        }
        Write-Host "uv $($uvArgs -join ' ')"
        & $Uv @uvArgs
        if ($LASTEXITCODE -ne 0) { throw "uv pip compile failed for $comp $plat" }
    }
}
Write-Host "Locks refreshed. Next: go run ./packaging/components/gen"
