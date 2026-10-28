# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
#
# Signs a release's update.json with minisign, on the maintainer's computer
# only (SPEC-ext "应用内更新"; internal/selfupdate/README.md). CI never holds
# the secret key.
#
#   powershell -NoProfile -ExecutionPolicy Bypass -File scripts\release\sign-update.ps1 `
#       -SecretKey D:\keys\lectureflow.key [-Manifest .\update.json] [-PublicKey .\lectureflow.pub] [-Minisign minisign]
#
# minisign asks for the key's password itself; this script never reads,
# prints or stores the password or the key. The trusted comment is
# "lectureflow update version:<version of update.json>", which the
# application checks. With -PublicKey the signature is verified afterwards.
# Writes <Manifest>.minisig next to update.json; upload that file to the
# release (see scripts/release/README.md).

param(
  [Parameter(Mandatory = $true)][string]$SecretKey,
  [string]$Manifest = "update.json",
  [string]$PublicKey = "",
  [string]$Minisign = "minisign"
)

$ErrorActionPreference = "Stop"

function Fail([string]$msg) {
  Write-Error "sign-update: $msg"
  exit 1
}

if (-not (Test-Path -LiteralPath $Manifest -PathType Leaf)) { Fail "no such file: $Manifest" }
if (-not (Test-Path -LiteralPath $SecretKey -PathType Leaf)) { Fail "the secret key file does not exist" }
if ($PublicKey -and -not (Test-Path -LiteralPath $PublicKey -PathType Leaf)) { Fail "no such file: $PublicKey" }
if (-not (Get-Command $Minisign -ErrorAction SilentlyContinue)) {
  Fail "minisign not found (install minisign 0.11 or later, or pass -Minisign PATH)"
}

try {
  $json = [System.IO.File]::ReadAllText((Resolve-Path -LiteralPath $Manifest).Path) | ConvertFrom-Json
} catch {
  Fail "$Manifest is not valid JSON"
}
if ($json.schema -ne 1 -or $json.app -ne "LectureFlow") { Fail "$Manifest is not a LectureFlow update.json (schema 1)" }
$version = [string]$json.version
if ($version -notmatch '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$') {
  Fail "invalid version '$version' in $Manifest"
}

$sig = "$Manifest.minisig"
$trusted = "lectureflow update version:$version"
Write-Host "Signing $Manifest (LectureFlow $version); minisign asks for the key's password."
& $Minisign -S -s $SecretKey -m $Manifest -x $sig -t $trusted -c "LectureFlow $version update.json"
if ($LASTEXITCODE -ne 0) { Fail "minisign failed (exit $LASTEXITCODE); nothing was signed" }

$lines = [System.IO.File]::ReadAllLines((Resolve-Path -LiteralPath $sig).Path)
if ($lines.Count -lt 4 -or $lines[2] -ne "trusted comment: $trusted") {
  Fail "$sig does not carry the trusted comment '$trusted'"
}
if ($PublicKey) {
  & $Minisign -V -p $PublicKey -m $Manifest -x $sig
  if ($LASTEXITCODE -ne 0) { Fail "the new signature does not verify with $PublicKey" }
}
Write-Host "Wrote $sig. Upload it to the draft release, e.g.:"
Write-Host "  gh release upload v$version $sig --repo OWNER/REPO"
