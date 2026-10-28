# Release tools for the in-app update

The in-app update (SPEC-ext "应用内更新"; format and contract in
`internal/selfupdate/README.md`) needs two files on every GitHub release:
`update.json` (what the release contains: versions, package URLs, sizes,
SHA-256) and `update.json.minisig` (its minisign signature). CI makes the
first; the maintainer makes the second on their own computer, because only
that computer holds the password-protected secret key. Nobody else, CI
included, ever gets the key.

| File | Where it runs | What it does |
|---|---|---|
| `make-update-json/` | CI (release workflow) | `go run ./scripts/release/make-update-json -dir out -version 3.2.0 -repo OWNER/REPO`: finds the four packages in `out/`, hashes them, writes `out/update.json` (unsigned) and checks it with the application's own parser. |
| `sign-update.ps1` | maintainer, Windows | Signs `update.json` with minisign; the trusted comment is `lectureflow update version:<version>`. |
| `sign-update.sh` | maintainer, macOS / Linux | The same in bash. |

## The update key

| | |
|---|---|
| Public key (`PublicKey` in `internal/selfupdate/keys.go`) | `RWRtYxdsG0+sD5yxDvxA4MOrUrK3WGrkwpejK2VfGHH3GibjDpES6nTX` |
| Key id, as minisign prints it (`PublicKeyID`) | `0FAC4F1B6C17636D` |
| Made | 2026-10-27, with minisign, on the maintainer's computer |
| Secret key | a password-protected minisign key file (`lectureflow.key`) on the maintainer's computer only, plus an offline backup |

Every build from 3.2.0 on trusts this key and no other. The `.pub` file's
first line names the key id (`untrusted comment: minisign public key
0FAC4F1B6C17636D`), and so does `minisign -V` for a signature made with it.
`TestBuiltinPublicKey` checks that `PublicKey`, `PublicKeyID` and this page
agree. Nothing secret appears here or anywhere in the repository: the secret
key file and its password stay with the maintainer.

## Releasing a version

1. Push the version tag; the release workflow builds the packages and creates
   a **draft** release with them and the unsigned `update.json`.
2. On the maintainer's computer, with minisign 0.11 or later installed (from
   <https://github.com/jedisct1/minisign/releases>, or a package manager such
   as `winget install jedisct1.minisign` / `brew install minisign` /
   `scoop install minisign`):

   ```
   gh release download v3.2.0 --repo ShouldBeQuincy/LectureFlow --pattern update.json --dir release-v3.2.0
   scripts/release/sign-update.sh -s ~/keys/lectureflow.key -m release-v3.2.0/update.json -p ~/keys/lectureflow.pub
   # Windows: powershell -NoProfile -ExecutionPolicy Bypass -File scripts\release\sign-update.ps1 `
   #   -SecretKey D:\keys\lectureflow.key -Manifest release-v3.2.0\update.json -PublicKey D:\keys\lectureflow.pub
   gh release upload v3.2.0 release-v3.2.0/update.json.minisig --repo ShouldBeQuincy/LectureFlow
   ```

   minisign asks for the key's password; the scripts never read, print or
   store the password or the key. `-p` / `-PublicKey` verifies the new
   signature right away.
3. Publish the release as a normal release: GitHub's
   `releases/latest/download/update.json` (what the stable channel asks)
   never points at a draft or a pre-release; a pre-release is found only by
   the beta channel (releases API). Installed copies find it at their next
   daily check.

Do not edit `update.json` after signing: any change, even one byte, makes
every installation refuse it. To change it, sign it again.

### Signing by hand

The scripts run exactly this (`3.2.0` = the `version` field of update.json):

```
minisign -S -s lectureflow.key -m update.json -x update.json.minisig -t "lectureflow update version:3.2.0" -c "LectureFlow 3.2.0 update.json"
```

- `-s` the secret key file, `-m` the file to sign, `-x` the signature file
  (next to update.json, uploaded as `update.json.minisig`), `-c` the
  untrusted comment (free text, not checked).
- `-t` the **trusted comment**, signed together with the signature:
  `lectureflow update version:<version>`. The version must equal update.json's
  `version`; more `key:value` fields may follow and are ignored
  (`lectureflow update version:3.2.0 channel:stable`). The short form
  `minisign -Sm update.json -s lectureflow.key` signs minisign's default
  trusted comment (`timestamp:… file:update.json …`), which every
  installation refuses (`selfupdate.version_mismatch`): always pass `-t`, or
  use the scripts, which also check the result.
- minisign 0.11 and later make prehashed signatures (`ED`); legacy ones
  (`-l`, `Ed`) are accepted as well. No other option is needed.

Check a signature (also after downloading it from the release) against the
built-in key, without any file:

```
minisign -V -P RWRtYxdsG0+sD5yxDvxA4MOrUrK3WGrkwpejK2VfGHH3GibjDpES6nTX -m update.json -x update.json.minisig
```

It must print `Signature and comment signature verified` and
`Trusted comment: lectureflow update version:3.2.0`.

## Making a key pair (done on 2026-10-27)

```
minisign -G -p lectureflow.pub -s lectureflow.key
```

Choose a strong password; keep `lectureflow.key` and its password only on the
maintainer's computer (and an offline backup). The base64 line of
`lectureflow.pub` (the second line, `RW…`) is `PublicKey` in
`internal/selfupdate/keys.go`, its key id `PublicKeyID`; `Repo` names the
repository. A build without a key (`PublicKey` empty, or built with
`-ldflags "-X lectureflow/internal/selfupdate.builtinPublicKey="`, as the
desktop end-to-end tests of the electron-updater path do) reports updates as
"not configured" and makes no request.

## Changing the key

An installation trusts only the key it was built with, and there is no way
to withdraw it from installed copies. So a new key reaches users through one
**bridge release** signed with the old key:

1. Make the new pair (`minisign -G -p lectureflow-2.pub -s lectureflow-2.key`,
   a new password). Its key id is in the first line of the `.pub` file.
2. Put the new base64 line into `PublicKey` and its key id into
   `PublicKeyID`, update the table above and the key paragraph of
   `internal/selfupdate/README.md` (`TestBuiltinPublicKey` fails until
   they agree), and release that version.
3. Sign this bridge release's update.json with the **old** key: installed
   copies verify it with the key they have, download it, and their own
   service program checks it again before it is installed. Keep it the latest
   stable release for a while.
4. Sign every later release with the new key only. A copy that missed the
   bridge sees the newer release, refuses its signature
   (`selfupdate.signature_invalid`, "signed with key …, not with the built-in
   key …") and has to install the bridge, or the new version, by hand once.

The bridge version cannot roll back to a package of the old key (its
`-selfupdate-previous` check uses the new key); only the shell's backup of the
installation brings back the previous version, so test the bridge release
with particular care. When the old secret key is **lost**, no bridge can be
signed: release the new key in a normal version and tell users to install it
by hand. When it may have been **stolen**, do the same at once and say so in
the release notes: whoever holds it can sign updates that installed copies
accept, until they have the version with the new key.

To check this package against the real minisign:
`go test -tags minisign -run RealMinisign ./internal/selfupdate/` (it makes a
throwaway key in a temporary directory).

## Release workflow steps (in `.github/workflows/release.yml`)

The workflow already has these steps. In the `draft-release` job, check out
the repository and set up Go **before**
downloading the installers (checkout cleans the workspace), then make
`update.json` before `gh release create` (which uploads `out/*`, so
`update.json` goes up with the packages):

```yaml
  draft-release:
    needs: build
    runs-on: ubuntu-latest
    permissions:
      contents: write
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
      - uses: actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e # v7.0.0
        with:
          go-version-file: go.mod
      - uses: actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333 # v8.0.2
        with:
          pattern: installers-*
          path: out
          merge-multiple: true
      - name: update.json (unsigned; the maintainer signs it, see scripts/release/README.md)
        run: |
          go run ./scripts/release/make-update-json -dir out \
            -version "${GITHUB_REF_NAME#v}" -tag "${GITHUB_REF_NAME}" -repo "${GITHUB_REPOSITORY}"
      - name: Create the draft release
        # unchanged
```

The build jobs already copy `*.exe`, `*.dmg` and `*.zip` into `out/`; the macOS
zip keeps electron-builder's default name (`LectureFlow-<version>-arm64-mac.zip`),
which `make-update-json` expects (it also accepts `…-arm64.zip`). Add
`-min-version X.Y.Z` when a release cannot be reached from older versions
directly, and `-mirror PREFIX=REPLACE` for mirror rules listed in update.json.
