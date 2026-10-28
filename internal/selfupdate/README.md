# In-app update: update.json, signatures and the contract with the desktop shell

This package is the Go side of the in-app update (SPEC-ext "应用内更新（不依赖自有服务器）").
It needs no server of our own: releases live on GitHub Releases, every release
carries a signed `update.json`, and the application trusts nothing but the
minisign public key compiled into it. Mirrors may therefore be added freely:
a mirror that changes a single byte is refused.

What the Go service does: check (once a day after a check that worked, sooner
after a failed one; can be switched off),
download this installation's package, accept offline updates from files, verify
everything, and leave the verified package in `{data_dir}/updates/` with a
`pending.json`. What the desktop shell (Electron) does: at the next start,
before the service starts, install the pending package, keep the previous
version, and go back to it when the new one fails. Electron's side is
described in "The updates directory" below.

This document is the **frozen** description of schema 1. Changes that old
versions could misread need a new `schema` number (old versions then report
`selfupdate.manifest_invalid {field: "schema"}` and the user installs by
hand).

## update.json (schema 1)

```json
{
  "schema": 1,
  "app": "LectureFlow",
  "version": "3.2.0",
  "channel": "stable",
  "released_at": "2026-11-02T10:00:00Z",
  "min_version": "3.0.0",
  "notes_url": "https://github.com/OWNER/REPO/releases/tag/v3.2.0",
  "platforms": {
    "windows-x64": [
      {
        "kind": "installer",
        "name": "LectureFlow-Setup-3.2.0-x64.exe",
        "url": "https://github.com/OWNER/REPO/releases/download/v3.2.0/LectureFlow-Setup-3.2.0-x64.exe",
        "size": 189123456,
        "sha256": "3f1d0c8a3e5b0e7f2a9c4d6b8e1f3a5c7e9b0d2f4a6c8e0b2d4f6a8c0e2b4d6f"
      },
      {
        "kind": "portable",
        "name": "LectureFlow-3.2.0-x64-portable.exe",
        "url": "https://github.com/OWNER/REPO/releases/download/v3.2.0/LectureFlow-3.2.0-x64-portable.exe",
        "size": 188987654,
        "sha256": "8b2e4f6a0c2e4a6c8e0b2d4f6a8c0e2b4d6f8a0c2e4b6d8f0a2c4e6b8d0f2a4c"
      }
    ],
    "darwin-arm64": [
      {
        "kind": "dmg",
        "name": "LectureFlow-3.2.0-arm64.dmg",
        "url": "https://github.com/OWNER/REPO/releases/download/v3.2.0/LectureFlow-3.2.0-arm64.dmg",
        "size": 201234567,
        "sha256": "0c2e4a6c8e0b2d4f6a8c0e2b4d6f8a0c2e4b6d8f0a2c4e6b8d0f2a4c6e8b0d2f"
      },
      {
        "kind": "zip",
        "name": "LectureFlow-3.2.0-arm64-mac.zip",
        "url": "https://github.com/OWNER/REPO/releases/download/v3.2.0/LectureFlow-3.2.0-arm64-mac.zip",
        "size": 198765432,
        "sha256": "e0b2d4f6a8c0e2b4d6f8a0c2e4b6d8f0a2c4e6b8d0f2a4c6e8b0d2f4a6c8e0b2"
      }
    ]
  },
  "mirrors": [
    {"prefix": "https://github.com/", "replace": "https://mirror.example.org/github/"}
  ]
}
```

| Field | Rule |
|---|---|
| `schema` | `1`. Any other value is refused. |
| `app` | `"LectureFlow"`. |
| `version` | Semantic version `MAJOR.MINOR.PATCH[-pre][+build]`, no `v`. Must equal the version in the signature's trusted comment. |
| `channel` | `stable` or `beta`. A `stable` release has no pre-release part. Stable users are only offered stable releases; beta users both. |
| `released_at` | RFC 3339. |
| `min_version` | Optional. Installations older than this cannot update directly: the release is shown (`available.direct = false`) but not downloaded; the user installs it by hand. Not above `version`. |
| `notes_url` | Optional, https. |
| `platforms` | Platform → packages. Platforms of 3.x: `windows-x64`, `darwin-arm64`. Each package: `kind` (lower case, `[a-z0-9][a-z0-9:_-]*`, unique per platform), `name` (a plain file name: no path, no leading dot, no `..`), `url` (http or https, no user info; packages are only downloaded over https, or http to this computer), `size` (1 byte to 4 GiB), `sha256` (64 lower-case hex digits). |
| `mirrors` | Optional. Every package URL that starts with `prefix` (ending in `/`) can also be fetched from `replace` + the rest of the URL. |

Kinds of 3.x: `installer` (Windows NSIS installer, per user), `portable` (Windows
portable executable), `dmg` (macOS disk image), `zip` (macOS: the `.app` in a
zip, what downloaded updates use). The installation picks its kind: Windows
installed → `installer`, Windows portable (`-portable`) → `portable`, macOS →
`zip` (an offline update may also bring the `dmg`).

Forward compatibility: unknown top-level fields, unknown platforms and unknown
kinds are ignored (but every listed package must still be well-formed).
Components stay in the component manifest shipped with each version
(`packaging/components/`); should update.json ever list them, it adds packages
of a new kind (for example `component:asr-cuda`), which 3.x ignores.

## Signature (minisign)

`update.json.minisig` is a [minisign](https://jedisct1.github.io/minisign/)
signature of the exact bytes of `update.json`, made by
`scripts/release/sign-update.{ps1,sh}` on the maintainer's computer:

```
untrusted comment: <anything; not signed>
<base64: "ED" (or legacy "Ed") | key id (8 bytes) | Ed25519 signature (64 bytes)>
trusted comment: lectureflow update version:3.2.0
<base64: global signature (64 bytes) over signature || trusted comment>
```

- Both signature kinds are accepted: `ED` (the default since minisign 0.11:
  Ed25519 over the BLAKE2b-512 hash of the file) and the legacy `Ed` (Ed25519
  over the file itself).
- The **trusted comment** is `lectureflow update` followed by space-separated
  `key:value` fields, exactly one of them `version:<version>`; other fields
  are allowed and ignored. minisign's default trusted comment
  (`timestamp:… file:…`) is refused.
- The public key is the constant `PublicKey` in `keys.go` (the base64 line of
  the `.pub` file). Since 2026-10-27 (3.2.0) it is the maintainer's key
  `RWRtYxdsG0+sD5yxDvxA4MOrUrK3WGrkwpejK2VfGHH3GibjDpES6nTX`, key id
  `0FAC4F1B6C17636D` as minisign prints it (`PublicKeyID`; changing it:
  scripts/release/README.md, "Changing the key"). **A build without a key
  is "not configured"** (`PublicKey` empty, or built with `-ldflags "-X
  lectureflow/internal/selfupdate.builtinPublicKey="`): the status says
  `not_configured` (`reason: no_public_key`), nothing is checked or
  downloaded, no error is shown, and the desktop shell keeps the older
  electron-updater path. `Repo` (`owner/name`) names the GitHub repository;
  while it is empty only the extra sources of the settings are asked.
- The secret key is a password-protected minisign key file on the maintainer's
  computer only. CI never holds it: CI builds an unsigned `update.json`, the
  maintainer signs it and uploads the `.minisig` (scripts/release/README.md).

Verification order (any failure refuses the whole update.json):

1. the `.minisig` parses (at most 16 KiB) and its key id is the built-in key's
   (`selfupdate.signature_invalid`);
2. the signature verifies over the raw bytes, then the global signature over
   the trusted comment (`selfupdate.signature_invalid`);
3. only now update.json is parsed and validated (`selfupdate.manifest_invalid {field}`);
4. the trusted comment's version equals `version` (`selfupdate.version_mismatch {signed, manifest}`);
5. **no downgrade**: `version` must be newer than the running version. A
   check that finds nothing newer reports `up_to_date`; an offline update
   that is not newer is refused (`selfupdate.not_newer {version, current}`); a
   `pending.json` that is not newer than the running version is dropped at
   start;
6. a package is accepted only when its size and SHA-256 equal the entry of
   update.json (`selfupdate.checksum {file}`).

Nothing is downloaded into, imported into, installed from or rolled back
from a data directory that other accounts of the computer can change (the
data directory or its `updates/` directory: a foreign owner, or an allow
entry with write rights for anyone but the user, SYSTEM, Administrators,
TrustedInstaller; on macOS / Linux group or others write access on a path
others can reach): download and import answer 409
`selfupdate.untrusted_dir {path, who}`, `-selfupdate-verify` exits 3. The
diagnostics page warns about such a data directory and offers to make it
private (`POST /api/system/data-dir/restrict`); LectureFlow makes the data
directories it creates or moves private from the start (internal/dirperm).

## Sources and mirrors

update.json is asked from every source below at the same time; of the
update.json files that verify, the **newest one the channel may offer**
wins (the first source in this order among equal versions). A source that
serves an old release, signed but stale, cannot hide a newer one another
source has. Sources are https; plain http is accepted only for this
computer (`127.0.0.1`, `::1`, `localhost`: a local cache), since on the way
anyone could see what is fetched and hold back newer releases.

1. GitHub (when `Repo` is set). Stable channel:
   `https://github.com/<repo>/releases/latest/download/update.json` and
   `….minisig` (GitHub's "latest" never points at a pre-release or a draft).
   Beta channel: first the public releases API
   (`https://api.github.com/repos/<repo>/releases?per_page=20`): among the
   published releases with both `update.json` and `update.json.minisig` assets
   (asset URLs must be this repository's `releases/download/` URLs), the one
   with the highest tag version; then the stable URL.
2. The extra sources of the settings (`selfupdate.sources`, at most 8), in
   order (https; a stored entry of an older version that is not is left
   out). Each is one of:
   - a proxy template with `{url}`: `https://proxy.example/{url}`; `{url}` is
     replaced with the GitHub URL, for update.json, its signature and the
     packages;
   - a URL ending in `.json`: that update.json, its signature at the same URL
     plus `.minisig`, the packages in the same directory;
   - any other https URL: a directory holding `update.json`,
     `update.json.minisig` and the packages under their `name`.

The channel is the existing setting `app.update_channel`.

Packages are downloaded from, in order: the source that delivered update.json,
the package's `url`, the other extra sources, then update.json's `mirrors`
rules; a URL that is plain http to another computer is skipped. Every
candidate serves the same bytes or is refused by the SHA-256.

**No expiry.** The signature carries no time, and update.json's
`released_at` is not compared with the clock: releases are signed by hand on
the maintainer's computer, not on a schedule, and an offline update may
rightly bring a release from months ago. A source that is stuck on an old
release can only withhold the newer one (never install anything unsigned or
older than the running version), and asking every source makes that
need all of them.

**Privacy.** Requests carry `User-Agent: LectureFlow` and nothing else that
could identify the user: no cookies (no cookie jar; `Cookie` is dropped on
redirects), no `Referer` (dropped on redirects), no installation id, no version
in the URL. The releases API request adds `Accept: application/vnd.github+json`.
Redirects are followed only to https, or to plain http on this computer, and
never from https down to http (also on the same host); the bundled aria2c
cannot be restricted like this (the signature and SHA-256 still decide).
Automatic checks run a day after a check that worked; after a failed one
sooner, backing off (`CheckInterval`): 1 h, 3 h, 6 h, then a day; while the
latest release has no update.json or signature (`no_manifest`) every 2 h for
about a day (12 times), then every 6 h, so that an update is picked up within
hours of the maintainer uploading the signature. A check by hand counts too;
the time, the failures in a row and the last failure are kept in kv
`selfupdate.last_check` (the status shows the failure again after a restart,
and the back-off goes on). A last check "in the future" (the clock was set
back) counts as made now. Automatic checks can be switched off
(`selfupdate.auto_check`). Nothing is downloaded automatically: the user starts
the download.

## Downloading

`Downloader` is the interface the application implements with the bundled
aria2c (internal/fetch); `HTTPDownloader` is the net/http fallback. Contract
(also in `download.go`):

- write to the given path (`updates/<version>/<name>.part`), resuming what is
  there; helper files must start with that path (`….part.aria2`) and are
  removed when done;
- the URLs are mirrors of one file, best first;
- send only `User-Agent: LectureFlow`;
- return nil once the file has the expected size; the updater then hashes it,
  deletes it on a mismatch (`selfupdate.checksum`), and renames it to
  `updates/<version>/<name>`;
- stop promptly when the context ends (cancel = `DELETE /api/selfupdate/download`;
  the partial file stays for the next attempt).

## Offline update ("从文件更新")

`POST /api/selfupdate/import` takes the files the user picked or dropped:
`update.json`, `update.json.minisig` and the package, in any order and under
any names (they are told apart by content), or **one zip** holding the three
(anywhere in the zip; `__MACOSX/` and dot files are ignored; the package is the
entry named like this installation's package, else the only other file). The
macOS package is itself a zip; given alone it is a package, not a bundle
(`selfupdate.import_incomplete {missing: "manifest"}`).

The user's files are copied once into `updates/.import-*/` (removed afterwards)
and everything else reads only that copy. The checks are those of a download,
without any network: signature, version binding, newer than the running
version, `min_version`, and a package of this platform whose size and SHA-256
match (on macOS the `zip` or the `dmg`). The channel is not checked: importing
a beta by hand is the user's choice.

## The updates directory (contract with Electron)

```
{data_dir}/updates/
  <version>/update.json            verified signature and manifest (Go)
  <version>/update.json.minisig
  <version>/<package name>         verified package (Go)
  <version>/<package name>.part    a download in progress (Go)
  pending.json                     Go → Electron: install this at the next start
  previous.json                    Go (Electron may add "backup"): what a failed update returns to
  result.json                      Electron → Go: the outcome of the last installation
  latest.json, latest.json.minisig the last verified check result (Go)
  .import-*/                       offline-update staging (Go)
  backup/                          Electron's copies of replaced installations (Go never touches it)
```

All JSON files are UTF-8, written to a temporary file in the same directory and
renamed over the target. Paths inside them are **relative to the updates
directory, with `/`** (the data directory can be moved).

### pending.json (written by Go; read and consumed by Electron)

Written only after every check passed (signature with the built-in key, version
binding, newer than `from_version`, size and SHA-256 of the package).

```json
{
  "schema": 1,
  "version": "3.2.0",
  "from_version": "3.1.0",
  "channel": "stable",
  "platform": "windows-x64",
  "kind": "installer",
  "file": "3.2.0/LectureFlow-Setup-3.2.0-x64.exe",
  "size": 189123456,
  "sha256": "3f1d0c8a3e5b0e7f2a9c4d6b8e1f3a5c7e9b0d2f4a6c8e0b2d4f6a8c0e2b4d6f",
  "manifest": "3.2.0/update.json",
  "signature": "3.2.0/update.json.minisig",
  "key_id": "8F2D0A6B3C1E4D57",
  "origin": "download",
  "verified_at": "2026-11-02T12:00:00Z"
}
```

`origin` is `download` or `file` (offline update). `key_id` is the key id as
minisign prints it.

### previous.json (written by Go together with pending.json)

Describes the running version, to return to when the update fails. Written
before pending.json.

```json
{
  "schema": 1,
  "version": "3.1.0",
  "platform": "windows-x64",
  "kind": "installer",
  "file": "3.1.0/LectureFlow-Setup-3.1.0-x64.exe",
  "size": 188000000,
  "sha256": "…",
  "backup": "backup/3.1.0",
  "created_at": "2026-11-02T12:00:00Z"
}
```

`file` is set when `updates/<running version>/` holds that version's verified
package (it was installed through the updater); otherwise it is absent and
Electron must back up the installation before replacing it, then rewrite
previous.json with `backup` (the only field Electron may change; keep the
others). Go keeps `backup` when it rewrites previous.json for the same version.

### result.json (written by Electron)

```json
{"version": "3.2.0", "from": "3.1.0", "outcome": "installed", "at": "2026-11-03T08:00:00Z"}
```

`outcome`: `installed`, `rolled_back` (the new version failed and the previous
one was restored) or `failed` (nothing was changed); `error`: optional short
English reason (shown under "查看详情"). Go only reads it (status
`last_result`).

### What Electron does at start (before starting the service)

1. Read `pending.json`. Ignore it (and delete it) unless `schema` is 1,
   `platform` and `kind` are this installation's, `version` is newer than the
   running version, and the file at `file` has `size` bytes and the SHA-256
   `sha256` (hash it again: the file may have been damaged since).
   Then run the installed (old) service program to check it all again with
   its built-in public key, signature included, and to copy the package
   where nothing else can change it:
   `lectureflow -selfupdate-verify <updates directory> -app-version <running version> -selfupdate-copy <temporary directory>`.
   It checks first that no other account can change the data directory,
   then reads only the updates directory, writes nothing there and prints
   one line on stdout; exit code 0 `ok version=X.Y.Z kind=K file=<relative
   path> sha256=<hex> copy=<absolute path>`: the package was copied, while
   it was hashed, into a new directory only this account may use
   (`<temporary directory>/lectureflow-update-<n>/<name>`; Windows: a
   protected DACL for the user, SYSTEM and Administrators; elsewhere 0700) —
   **run only that copy** (sec-update M3: nothing can swap the file between
   the check and the start), and remove it when done (an installer still
   runs from it while the app quits: a later start removes copies older
   than ten minutes); 1 `invalid <code>: <reason>` (do not install; delete
   `pending.json`); 2 `not_configured: …` (a build without a public key:
   nothing can have been verified, do not install); 3 `untrusted_dir: …`
   (other accounts can change the data directory: do not install, keep
   `pending.json` for a start after the permissions were tightened). Without
   `-selfupdate-copy` it writes nothing and the ok line has no `copy=`.
2. When previous.json has no `file`, back up the installation into
   `backup/<running version>/` (Windows portable: the executable; macOS: move
   the `.app`; Windows installed: the installation directory) and record it as
   `backup`. The desktop shell also records the SHA-256 of the backup's
   whole tree in its own profile (outside the data directory), and later
   uses only a backup that still has it.
3. Record the installation (versions and the installation's place) in the
   shell's profile, rename `pending.json` to `applying.json` (so a crash
   during the installation is noticed), then install:
   - `installer`: run it silently (NSIS `/S`), then start the installed app;
   - `portable`: replace the portable executable (`PORTABLE_EXECUTABLE_FILE`)
     with the new one;
   - `zip`: unpack the `.app` next to the current one, swap the two, keep the
     old one as the backup; `dmg`: mount, copy the `.app`, swap likewise.
4. The new version's Electron, finding `applying.json` with its own version,
   waits until its service is healthy, then deletes `applying.json` and writes
   result.json `installed`. When the new version does not become healthy
   (for example two starts in a row), restore from previous.json (`file`: run
   or copy that package; `backup`: put the backup back), delete
   `applying.json` and write `rolled_back`. A rollback runs nothing it has
   not checked (sec-update M2): `applying.json` counts only when the shell
   recorded that installation and its `target` is this installation's place;
   the package is checked like an update by the installed service program,
   `lectureflow -selfupdate-verify <updates directory> -selfupdate-previous
   -app-version <running version> -selfupdate-copy <temporary directory>`
   (previous.json's `file`: the signature of `<version>/update.json`, the
   version binding, older than the running version, size and SHA-256; the
   same exit codes; exit 1 `selfupdate.not_available` when previous.json
   names no package), and run from its copy; a backup only when its tree
   still has the recorded hash. Exit 3 (or no answer at all): no rollback.

Go drops a `pending.json` whose version is not newer than the running version
(the update was installed), never reads or writes `applying.json`, and removes
at start: offline-update staging directories and version directories that are
neither the running version, the pending one, previous.json's, nor the
available one being downloaded. Other names (`backup/`) are left alone.

## HTTP API

| Request | Answer |
|---|---|
| `GET /api/selfupdate` | `Status` |
| `POST /api/selfupdate/check` | 200 `Status` after the check; a failed check is `error` (`selfupdate.signature_invalid`, `version_mismatch`, `manifest_invalid`, `no_manifest` (the release has no update.json or signature: HTTP 404), `unreachable`, the most telling of all sources; `detail` lists every source's answer, one line each) and `state: "error"` unless an update can still be installed (see below); 409 `not_configured`, `busy` |
| `POST /api/selfupdate/download` | 202 `Status` (`downloading`); 409 `not_configured`, `busy`, `not_available`, `too_old`, `no_package`, `untrusted_dir {path, who}` |
| `DELETE /api/selfupdate/download` | 200 `Status`: the download is cancelled (the partial file stays) |
| `POST /api/selfupdate/import` | Desktop mode JSON `{"paths": [...]}` (or `{"path"}`), else multipart with one to three `file` parts. 200 `Status` (`pending`); 400 `signature_invalid`, `version_mismatch`, `manifest_invalid`, `checksum`, `import_incomplete {missing}`, `import_invalid {file}`; 409 `not_newer`, `too_old`, `no_package`, `not_configured`, `busy`, `untrusted_dir {path, who}`; the upload and path codes of the other upload endpoints |
| `DELETE /api/selfupdate/pending` | 200 `Status`: the pending update is dropped, with its files (`pending.json` and `updates/<version>/`; the running version's files and `backup/` stay) |
| `GET /api/selfupdate/settings` | `{"auto_check": true, "sources": []}` |
| `PATCH /api/selfupdate/settings` | `{"auto_check"?, "sources"?}` → 200 settings; 400 `selfupdate.sources_invalid {index}` |

Every status change is announced as SSE `changed {kind: "selfupdate"}` (download
progress at most once a second).

`Status`:

```
state            not_configured | idle | checking | up_to_date | available |
                 downloading | importing | pending | error
reason           not_configured: no_public_key | web_mode | dev_build |
                 unsupported_platform | no_source
current_version, channel (stable | beta), platform, kind, auto_check, last_check
next_check       the next automatic check (auto_check on, after a first check)
available        {version, channel, released_at, notes_url, min_version, direct,
                  file, size, source}   a newer version (newer than a pending one too)
progress         {bytes_done, bytes_total}       while downloading
pending          {version, file, size, origin, verified_at}
rollback         {version, package}   previous.json; package: a package or backup exists
last_result      result.json
error            {code, params, detail}          the last check or download failed
```

`available` and `pending` may both be present (a newer release appeared after
the download); the state is then `available`.

An update that can be installed comes first: a failed check leaves the
version found before `available` and a downloaded one `pending`, with the
failure in `error` (a note). `state: "error"` is a failed download of the
available version (`download_failed`, `checksum`, `io_failed`;
`IsDownloadError`), or a failed check when nothing can be installed. A
successful offline update clears the error.

## Releasing

See `scripts/release/README.md`: `make-update-json` (CI) builds the unsigned
update.json from the release files; `sign-update.ps1` / `sign-update.sh` sign it
on the maintainer's computer (by hand: `minisign -S -s lectureflow.key -m
update.json -x update.json.minisig -t "lectureflow update version:X.Y.Z"`;
minisign's default trusted comment is refused).

Without network a check fails with `selfupdate.unreachable`; while the latest
release carries no update.json or no signature (between publishing and
uploading the signature), with `selfupdate.no_manifest` ("the update
information is not ready yet", not a network problem). The settings show it,
the workbench does not, and the automatic checks back off as above (kept
across restarts; TestBuiltinKeyOfflineBacksOff).
