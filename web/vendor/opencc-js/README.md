# opencc-js (vendored)

Third-party code, copied unchanged. Do not edit these files; replace them with
a newer release instead (and update this note).

- **What**: [opencc-js](https://github.com/nk2028/opencc-js), the JavaScript
  implementation of Open Chinese Convert (OpenCC).
- **Version**: 1.4.2, from the npm registry (`opencc-js-1.4.2.tgz`,
  integrity `sha512-EdcspgdROiCCZDM+jZR6Pa670xW+EW2QhctPBQ8gRjg6lrx4IHEl+2s+kt5izk/TkxVRgdWuFgaGkaEdIUfdaw==`).
- **Files** (from the package, unchanged):

  | file | from | sha512 |
  |---|---|---|
  | `cn2t.js` | `dist/umd/cn2t.js` | `99fd8630c9a4adfe046cd261c7a08faffef9efa1bc7b855bdc44ea28366238584a8d65fb912de6db36e44c9ee4b2db2e3d7dabec44d78e1cdea269c90a80b259` |
  | `t2cn.js` | `dist/umd/t2cn.js` | `ca4c8237314f82c4d248b2aec70c6a16c121b0c40f0fbdec0f2d9c7b439ce1594ca50933cd774bc68b1b8ed2f07594e556ee7a510666322397f58bc53a9b465a` |
  | `LICENSE` | `LICENSE` | |
  | `LICENSES/Apache-2.0.txt` | `LICENSES/Apache-2.0.txt` | |
  | `THIRD_PARTY_LICENSES.md` | `THIRD_PARTY_LICENSES.md` | |

- **License**: the code is MIT (`LICENSE`, Copyright (c) 2020-2021 The nk2028
  Project); the bundled dictionary data is generated from OpenCC's
  `opencc-data` and is under the Apache License 2.0 (`LICENSES/Apache-2.0.txt`,
  `THIRD_PARTY_LICENSES.md`). Both allow redistribution with these notices,
  which is why they are kept next to the code and embedded with it.
- **Use**: SPEC-v2 P14.6, Traditional-Chinese display of lecture content
  (`web/zh_display.js`). The UMD builds are loaded on demand with a `<script>`
  element, only when a Traditional display mode is in use: `cn2t.js`
  (Simplified → Traditional, presets `cn → twp` and `cn → hk`) for display and
  downloads, `t2cn.js` (Traditional → Simplified, `tw/hk → cn`) for search
  terms. Nothing is fetched from a CDN; the scripts make no network requests
  and use no `eval` / `new Function`.
- **Checks**: `web/i18n_check_test.go` (hard-coded UI text) skips `web/vendor/`,
  because the dictionaries are Chinese text by design and are not UI strings.
