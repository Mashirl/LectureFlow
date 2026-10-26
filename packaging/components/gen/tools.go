// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"lectureflow/internal/components"
)

// Tool components (kind "tool", 3.1.0: gcloud, the Google Cloud CLI) are
// written out in full in manifest.src.json, without a gen block: Google
// publishes the SHA-256 of the current release only (the "versioned
// archives" page), not of a pinned older one, so the archive's SHA-256 is
// computed once from the download, compared with Google's table while that
// version is current, and pinned together with the size (see README.md,
// "Tools"). gen checks with a HEAD request that every URL still serves
// that many bytes, and never builds an offline archive of a tool: the
// component manager downloads it from its vendor on the user's computer,
// LectureFlow does not redistribute it.

// kindOf returns the kind of a component of the manifest source ("" when
// it is not there: models are generated from the models list).
func kindOf(src *srcManifest, id string) string {
	for _, raw := range src.Components {
		var c struct {
			ID   string `json:"id"`
			Kind string `json:"kind"`
		}
		if json.Unmarshal(raw, &c) == nil && c.ID == id {
			return c.Kind
		}
	}
	return ""
}

// toolRefusal explains why a tool gets no offline archive ("" = not a tool).
func toolRefusal(src *srcManifest, id string) string {
	if kindOf(src, id) != components.KindTool {
		return ""
	}
	return "it is a tool that the component manager downloads from its vendor on the user's computer " +
		"(packaging/licenses: not distributed by LectureFlow); an offline archive would redistribute it"
}

// headSize is the Content-Length a HEAD request reports (-1 = none).
var headSize = func(u string) (int64, error) {
	req, err := http.NewRequest(http.MethodHead, u, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("HEAD %s: HTTP %d", u, resp.StatusCode)
	}
	return resp.ContentLength, nil
}

// checkTools checks every file of the tool components: an absolute URL that
// still serves the pinned number of bytes.
func checkTools(m *components.Manifest) error {
	var errs []string
	for _, c := range m.Components {
		if c.Kind != components.KindTool {
			continue
		}
		plats := make([]string, 0, len(c.Platforms))
		for p := range c.Platforms {
			plats = append(plats, p)
		}
		sort.Strings(plats)
		for _, plat := range plats {
			for _, f := range c.Platforms[plat].Files {
				if !strings.HasPrefix(f.URL, "https://") {
					errs = append(errs, fmt.Sprintf("%s %s: %s is not an https URL of the vendor", c.ID, plat, f.URL))
					continue
				}
				n, err := headSize(f.URL)
				switch {
				case err != nil:
					errs = append(errs, fmt.Sprintf("%s %s: %v", c.ID, plat, err))
				case n != f.Size:
					errs = append(errs, fmt.Sprintf("%s %s: %s has %d bytes, the manifest pins %d (a new upstream build? see README.md, Tools)", c.ID, plat, f.URL, n, f.Size))
				}
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("tools: %s", strings.Join(errs, "; "))
	}
	return nil
}
