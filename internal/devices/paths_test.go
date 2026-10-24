// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package devices

import (
	"os"
	"path/filepath"
	"testing"
)

// The locations keep the 3a names (lead ruling 3: the existing NPU compile
// caches are reused).
func TestPaths(t *testing.T) {
	data := filepath.Join("D:", "x", "data")
	root := filepath.Join(data, "models", "openvino")
	if ModelRoot(data) != root {
		t.Fatalf("root %s", ModelRoot(data))
	}
	if got := ModelDir(data, "large-v3-turbo", "fp16"); got != filepath.Join(root, "whisper-large-v3-turbo-fp16") {
		t.Fatalf("model dir %s", got)
	}
	if got := CacheDir(data, "NPU", "large-v3-turbo", "fp16"); got != filepath.Join(root, "cache", "NPU-large-v3-turbo-fp16") {
		t.Fatalf("cache dir %s", got)
	}
	if HFHome(data) != filepath.Join(root, "hf_cache") || HFID("large-v3-turbo") != "openai/whisper-large-v3-turbo" {
		t.Fatal("hf")
	}
}

func TestModelReadyAndCompileCached(t *testing.T) {
	data := t.TempDir()
	if ModelReady(data, "small", "int8") {
		t.Fatal("ready without a directory")
	}
	dir := ModelDir(data, "small", "int8")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i, f := range ModelFiles {
		if ModelReady(data, "small", "int8") {
			t.Fatalf("ready with %d of %d files", i, len(ModelFiles))
		}
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if !ModelReady(data, "small", "int8") || ModelReady(data, "small", "fp16") {
		t.Fatal("ready")
	}
	// An empty file (an interrupted copy) is not ready.
	if err := os.WriteFile(filepath.Join(dir, "openvino_decoder_model.bin"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if ModelReady(data, "small", "int8") {
		t.Fatal("ready with an empty weight file")
	}

	if CompileCached(data, "NPU", "small", "int8") || !CompileCached(data, "CPU", "small", "int8") {
		t.Fatal("cache before compiling")
	}
	cache := CacheDir(data, "NPU", "small", "int8")
	if err := os.MkdirAll(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if CompileCached(data, "NPU", "small", "int8") {
		t.Fatal("cached without a blob")
	}
	if err := os.WriteFile(filepath.Join(cache, "17025243894867294591.blob"), []byte("blob"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !CompileCached(data, "NPU", "small", "int8") || CompileCached(data, "GPU", "small", "int8") {
		t.Fatal("cached per device")
	}
}
