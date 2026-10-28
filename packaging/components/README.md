# Components: manifest, lock files and offline archives

LectureFlow's installer carries only the core (SPEC-v3 V3). Python, the
transcription back ends, the Whisper models and the Google Cloud CLI (3.1.0)
are *components* that the component manager (`internal/components`)
downloads on first start, checks against SHA-256 and installs under
`{data_dir}/components/`.

| File | What it is |
|---|---|
| `components.json` | The manifest shipped with the application (copied to `resources/components.json` at build time, together with `locks/`). **Generated — do not edit by hand.** |
| `manifest.src.json` | The hand-written source of `components.json`: versions, sources, presets, which python-build-standalone release, which Hugging Face repositories at which revisions. |
| `requirements/*.in` | Top-level pins of each Python environment (`python` = the base venv with pypdf; `asr-cuda`, `asr-openvino`, `asr-mlx`). `asr-cuda.constraints.txt` pins the transitive versions of the environment that runs on the development machine. |
| `locks/{component}-{platform}.txt` | `uv pip compile --generate-hashes` output, one per component and platform. Installed with `uv pip sync --require-hashes`. |
| `refresh-locks.ps1` | Regenerates the lock files with the pinned uv. |
| `gen/` | `go run ./packaging/components/gen` — fills in hashes and sizes and writes `components.json`; with `-offline` it also builds offline archives. |
| `i18n/` | Proposed UI texts not yet merged by the lead (error codes added later, e.g. `components.in_use_by_task`); the component tests accept them until they are merged into `web/i18n`. |
| `.gitattributes` | Lock files and `components.json` are hashed byte for byte: no line-ending conversion. |

## Manifest format (schema 1)

```jsonc
{
  "schema": 1,                 // a higher schema is refused (components.manifest_schema)
  "serial": 1,                 // a downloaded manifest replaces the current one only with a larger serial
  "published": "2026-10-24",
  "app_version": "3.0.0",      // the application version this manifest was published with
  "compat": ">=3.0.0 <4.0.0",  // application versions that may use it (">=", ">", "<=", "<", "=", "||")
  "update_url": "",            // where CheckManifest looks for a newer manifest ("" = none yet)
  "sources": {                 // URL placeholders and their sources
    "pypi":   { "primary": "pypi",   "default": {...}, "mirrors": [...] },  // {pypi} package index, {python} runtime archives
    "models": { "primary": "models", "default": {...}, "mirrors": [...] },  // {models} Hugging Face endpoint
    "files":  { "primary": "files",  "default": {...} }                     // {files} project release files
  },
  "components": [ Component, ... ],
  "presets": { "default", "tiers", "providers", "free" }   // onboarding step 4 (V4.2)
}
```

A source is `{id, name_key, url, region, values: {placeholder: base URL}}`. A
mirror overrides only the placeholders it lists; `region: "cn"` marks the
mirror onboarding suggests to users in mainland China. The user picks a
source per group (`default`, a mirror id, or a custom URL that replaces the
group's `primary` placeholder). A failing mirror falls back to the default
source automatically and the task says so (`components.mirror_fallback`).

Component:

```jsonc
{
  "id": "asr-cuda",                // core | python | asr-* | model:{backend}:{name}:{precision}
  "version": "1.0.0",              // semver of the component (not of the upstream software)
  "upstream": "faster-whisper 1.2.1, ...",
  "kind": "venv",                  // python-runtime | venv | model | binary | tool
  "bundled": false,                // true only for core (part of the installer)
  "name_key": "...", "desc_key": "...",
  "depends": ["python"],
  "backend": "faster-whisper",     // venv: Python(backend); model: its back end
  "model": "...", "precision": "...",          // model components
  "license": "MIT", "license_note": "...", "source": "https://...", "revision": "<git sha>",
  "min_app": "3.1.0",              // optional
  "unavailable": "",               // a reason code: such a component is never installed
  "check_imports": ["faster_whisper", "ctranslate2"],   // the post-install check
  "check_args": ["--version"],     // tool: the post-install check runs exe with these arguments
  "check_env": {"CLOUDSDK_CONFIG": "{tmp}"},             // tool: variables of the check only
  "platforms": {
    "windows-x64": {
      "download_size": 0, "installed_size": 0, "installed_size_estimated": true,
      "files": [ { "name", "url": "{models}/...", "sha256", "size", "unpack": "tar.gz|zip|",
                   "strip": "google-cloud-sdk",          // archives: the top-level directory to unpack the contents of
                   "exclude": ["lib/x/"] } ],            // archives: paths (after strip) not unpacked; "dir/" or a file
      "exe": "python/python.exe",  // python-runtime: interpreter inside the archive; tool: its executable below tool/
      "depends": ["python"],       // dependencies on this platform only (added to the component's)
      "env": { "CLOUDSDK_PYTHON": "{exe:python}" },     // tool: variables it runs with ({dir}, {exe:ID})
      "lock": { "path": "locks/asr-cuda-windows-x64.txt", "url": "", "sha256", "packages" },
      "offline": { "name", "sha256", "size", "url" },   // offline archive (ImportFile)
      "min_os": "13.0",
      "max_path": 123              // longest file path inside the installed component, relative to
                                   // components/{dir}/{version}/ (measured; Windows path length check)
    }
  }
}
```

### The core component's bundled files

`core` is part of the installer (`bundled: true`): it is never downloaded,
and the validation skips its builds. Its `platforms` record the bundled
third-party programs whose SHA-256 is known before packaging, for the
diagnostics to compare with the file in use: today aria2c (SPEC-ext) on
Windows, `files: [{"name": "bin/aria2c.exe", "url": "", "sha256", "size"}]`
relative to the resources directory (`upstream` names the version, `aria2c
--version` reports it). The values come from
`desktop/packaging/third-party.json` (`bin_sha256` / `bin_size`);
`desktop/scripts/stage.js` fails when the staged file differs and
`desktop/test/unit/build.test.js` when the two files disagree. macOS has no
entry (built in CI and re-signed by electron-builder: no SHA-256 is known
before packaging). See `packaging/aria2c/README.md`.

### Installed sizes and path lengths of environments

`installed_size` of a venv or python component is an estimate (`installed_size_estimated`):
wheel bytes × a venv factor, plus the runtime archive × a runtime factor. The defaults are
`installed_factor` at the top of `manifest.src.json`; a component's `gen` block can override
them per platform (`installed_factor`, `runtime_factor`) with a ratio measured on a real
installation, because it differs a lot between environments. Measured on Windows 11 in the
V12.2 acceptance (bytes recorded in `state.json` right after installing):

| Component | Wheels (+ archive) | Installed | Factor used | Longest path below `{version}/` |
|---|---:|---:|---|---:|
| python 1.0.0 | 4 480 977 + 22 019 610 | 79 170 228 | venv 1.7 (default), runtime 3.3 | 113 |
| asr-cuda 1.0.0 | 1 461 190 705 | 2 377 577 201 | 1.65 (measured 1.63) | 123 |
| asr-openvino 1.0.0 | 102 759 934 | 315 083 908 | 3.1 (measured 3.07) | 105 |

`max_path` (the `gen` block's `max_path` per platform, copied to the build) is the longest file
path inside the installed component relative to its version directory, measured on the same
installation (`find . -type f` in each `{version}` directory). The component manager adds
`components/{dir}/{version}-r99/` and the data directory and refuses, on Windows, an install
whose total would exceed 250 characters (`components.path_too_long`): Python cannot load DLLs
from longer paths unless the system enables long paths. Model and binary builds need no value
(their file names are listed). After refreshing a lock file, install the component once and
measure both numbers again.

Model ids follow `hwdetect`: faster-whisper precisions are compute types
(`float16`, `int8_float16`; both ids carry the same CTranslate2 files and the
manager hard-links them instead of downloading twice), OpenVINO uses `fp16` /
`int8`, MLX `fp16`.

## Refreshing

1. uv: the lock files are made with **uv 0.13.1**. Download
   `uv-x86_64-pc-windows-msvc.zip` from
   https://github.com/astral-sh/uv/releases/tag/0.13.1, check it against the
   release's `.sha256` file
   (`3b2661a463a5baea7f5076ea2b25ab71d77f36a6c0d809e5350651792f80a672`) and
   unpack it to `tools/uv/` (git-ignored).
2. Edit `requirements/*.in` (and `manifest.src.json` for new components,
   models or revisions), then
   `powershell -NoProfile -ExecutionPolicy Bypass -File packaging/components/refresh-locks.ps1`.
   macOS locks are cross-compiled (`--python-platform aarch64-apple-darwin`,
   deployment target 13.0); `--only-binary :all:` keeps source builds out.
3. `go run ./packaging/components/gen` (network): SHA-256 of the
   python-build-standalone archives from the release's `SHA256SUMS`
   (cross-checked with the GitHub release API), wheel sizes from the PyPI
   JSON API (the wheel uv would pick for the platform among the locked
   hashes), model files from the Hugging Face API at the pinned revision
   (`?blobs=true`: LFS files carry their SHA-256; small files are fetched and
   checked against their git blob id). Model weights are not downloaded. An
   entry without a reliable hash is written with `"unavailable": "no_hash"`
   instead of a guess.
4. Bump `serial` (and component `version`s whose content changed) in
   `manifest.src.json` when publishing a manifest that replaces an older one.
5. `go test ./internal/components/...` checks the shipped manifest (valid,
   lock hashes, every hwdetect id present, every i18n key has a text).

## Tools (kind `tool`): gcloud

`gcloud` is Google's own Google Cloud CLI, which LectureFlow runs to sign in
to Google for Colab (3.1.0). The component manager downloads Google's
archive from `dl.google.com` on the user's computer, checks its SHA-256,
unpacks it into `components/gcloud/{version}/tool/` (the archive's
`google-cloud-sdk/` directory is stripped; it is unpacked into `.u/` and
renamed when complete) and runs `gcloud --version` with its own temporary
`CLOUDSDK_CONFIG` as the post-install check (it replaces the worker
self-test of an update). `Manager.ToolPath("gcloud")` is the executable
(`tool\bin\gcloud.cmd`, `tool/bin/gcloud`); `ToolEnv` the variables to run
it with (`config.Config.GcloudPath` / `GcloudEnv`: the component first, else
the `gcloud` on PATH).

| Platform | Archive | Python |
|---|---|---|
| windows-x64 | `google-cloud-sdk-{v}-windows-x86_64-bundled-python.zip` | its bundled Python (`CLOUDSDK_PYTHON={dir}/platform/bundledpython/python.exe`) |
| darwin-arm64 | `google-cloud-cli-{v}-darwin-arm.tar.gz` | none in Google's macOS archive (its installer would `sudo installer` a python.org package): the build depends on the `python` component and runs with `CLOUDSDK_PYTHON={exe:python}` |

Pinned: Google Cloud CLI **590.0.1** (2026-10-23).

| Platform | Bytes | SHA-256 | Installed bytes | `max_path` |
|---|---:|---|---:|---:|
| windows-x64 | 104 431 171 | `38dad2c60c8507a2042e419ac9293e66b9f96156226c6cda92dcb1c4e8c65cf2` (equal to Google's table) | 388 078 184 | 140 |
| darwin-arm64 | 54 234 180 | `02f20618f43b9147e049a73fec37ab71b586b0dc6a242fa6ca835229cb5e542f` | 324 839 056 + `__pycache__` (330 000 000, estimated) | — |

Google's versioned-archives page lists the SHA-256 of the *unversioned*
macOS file (`google-cloud-cli-darwin-arm.tar.gz`, `71a0aab5…`); the
versioned file differs only in its gzip header (the decompressed tar is
byte-identical, `c8f7bd93…`), and its MD5 equals the one Google's storage
reports (`x-goog-hash`, `cloud-sdk-release` bucket).

**Windows path length.** The complete Windows tree needs 152 characters
below the version directory, which leaves only 69 characters for the data
directory (`TestShippedManifestPathNeeds` asks for 80). The Windows build
therefore leaves out (`exclude`) the few directories whose paths are longer:
two Vertex AI "semantic governance" API clients, the Privileged Access
Manager schemas, a v1alpha1 commerce procurement client and gsutil's copy of
the google-auth tests (180 files, 2 MB; only `gcloud ai`, `gcloud pam`,
`gcloud commerce-procurement` subcommands that LectureFlow never runs use
them). `max_path` 140 is measured without them.

Refreshing to a newer release:

1. Download both archives (versioned names), compute the SHA-256 and compare
   the Windows one with
   https://docs.cloud.google.com/sdk/docs/downloads-versioned-archives
   (while that version is current), the macOS one via the decompressed tar or
   the storage MD5 as above. Check that the Windows archive still has
   `platform/bundledpython/` and whether the macOS one gained a Python.
2. Edit the `gcloud` entry of `manifest.src.json` (URL, `sha256`, `size`,
   `download_size`, `upstream`, `revision`, a new component `version`).
3. Measure: `go test -tags realnet -run TestRealNetGcloud -v ./internal/components/`
   on Windows and on the Mac (`LF_REALNET_UV=$(which uv)`) installs it,
   runs `--version`, the sign-in up to its link (`--no-launch-browser`, empty
   input: nothing signs in) and logs the installed size and the longest path;
   update `installed_size`, `max_path` and `exclude`.
4. `go run ./packaging/components/gen` (it checks every tool URL with a HEAD
   request against the pinned size) and `make.ps1 audit`.

A tool never gets an offline archive (`gen -offline` refuses it, `licaudit`
fails an `offline` entry): it is downloaded from its vendor, LectureFlow does
not redistribute it. THIRD_PARTY_NOTICES.md lists it in section 10.

## Offline archives

`go run ./packaging/components/gen -offline python,model:openvino:small:int8 -offline-out dist/offline`
downloads everything a component needs and writes, per platform,
`{dir}-{version}-{platform}.zip`.

Components whose lock file contains a Python package that
`packaging/licenses/allowlist.json` accepts only with scope `user-fetched`
(today **asr-cuda**: the NVIDIA libraries, av, ctranslate2; **asr-mlx**:
regex) never get an offline archive: those packages are acceptable only
because the component manager downloads them on the user's computer, and an
archive would distribute them. `gen -offline` refuses such a component before
downloading anything, and drops an offline entry an earlier `components.json`
still had for one (`licaudit` reports such an entry as a violation).

Archive layout:

```
component.json   {"schema": 1, "id", "version", "platform"}
files/{name}     the build's files (models, the runtime archive)
wheels/*.whl     venv and python components: the lock's wheels for the platform
lock.txt         the lock file
```

The archive is deterministic (sorted entries, stored, fixed times); its
SHA-256 and size go into the component's `offline` entry, and later runs of
`gen` keep that entry while the component's content is unchanged. The
manager accepts an archive only when its SHA-256 equals an `offline` entry of
the current manifest (`components.import_checksum` otherwise) and installs
venvs from it with `uv pip sync --offline --no-index --find-links`. It first
copies the archive to `components/.imports/` (hashing the copy as it writes)
and reads only that copy afterwards, so replacing the original file during
the import changes nothing; every extracted file is checked again against its
`sha256` in the manifest before it is moved into the component.
