#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
#
# Signs a release's update.json with minisign, on the maintainer's computer
# only (SPEC-ext "应用内更新"; internal/selfupdate/README.md). CI never holds
# the secret key.
#
#   scripts/release/sign-update.sh -s ~/keys/lectureflow.key [-m update.json] [-p lectureflow.pub] [-M minisign]
#
# minisign asks for the key's password itself; this script never reads,
# prints or stores the password or the key. The trusted comment is
# "lectureflow update version:<version of update.json>", which the
# application checks. With -p the signature is verified afterwards.
# Writes <update.json>.minisig next to update.json; upload that file to the
# release (see scripts/release/README.md).
set -euo pipefail

secret="" manifest="update.json" pubkey="" minisign="minisign"
usage() { echo "usage: $0 -s SECRET_KEY [-m update.json] [-p PUBLIC_KEY] [-M minisign]" >&2; exit 2; }
fail() { echo "sign-update: $*" >&2; exit 1; }
while getopts "s:m:p:M:h" opt; do
  case "$opt" in
    s) secret="$OPTARG" ;;
    m) manifest="$OPTARG" ;;
    p) pubkey="$OPTARG" ;;
    M) minisign="$OPTARG" ;;
    *) usage ;;
  esac
done
shift $((OPTIND - 1))
[ $# -eq 0 ] && [ -n "$secret" ] || usage

[ -f "$manifest" ] || fail "no such file: $manifest"
[ -f "$secret" ] || fail "the secret key file does not exist"
[ -z "$pubkey" ] || [ -f "$pubkey" ] || fail "no such file: $pubkey"
command -v "$minisign" >/dev/null 2>&1 || fail "minisign not found (install minisign 0.11 or later, or pass -M PATH)"

grep -q '"schema"[[:space:]]*:[[:space:]]*1[,[:space:]]' "$manifest" && grep -q '"app"[[:space:]]*:[[:space:]]*"LectureFlow"' "$manifest" \
  || fail "$manifest is not a LectureFlow update.json (schema 1)"
version="$(grep -o '"version"[[:space:]]*:[[:space:]]*"[^"]*"' "$manifest" | head -n 1 | sed 's/.*"\([^"]*\)"$/\1/')"
echo "$version" | grep -Eq '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$' \
  || fail "invalid version '$version' in $manifest"

sig="$manifest.minisig"
trusted="lectureflow update version:$version"
echo "Signing $manifest (LectureFlow $version); minisign asks for the key's password."
"$minisign" -S -s "$secret" -m "$manifest" -x "$sig" -t "$trusted" -c "LectureFlow $version update.json" \
  || fail "minisign failed; nothing was signed"

[ "$(sed -n 3p "$sig" | tr -d '\r')" = "trusted comment: $trusted" ] || fail "$sig does not carry the trusted comment '$trusted'"
if [ -n "$pubkey" ]; then
  "$minisign" -V -p "$pubkey" -m "$manifest" -x "$sig" || fail "the new signature does not verify with $pubkey"
fi
echo "Wrote $sig. Upload it to the draft release, e.g.:"
echo "  gh release upload v$version $sig --repo OWNER/REPO"
