// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package remote

import (
	"strings"
	"testing"
)

func testKit() KitSpec {
	lock := "a==1 \\\n    --hash=sha256:" + strings.Repeat("0", 64) + "\n"
	py := []byte("print(1)")
	return KitSpec{Version: "3.1.0", Engine: EngineMLX,
		Python:  KitArchive{URL: "https://example.org/p.tar.gz", SHA256: strings.Repeat("1", 64), Size: 10, Exe: "python/bin/python3"},
		Lock:    KitLock{Name: "asr-mlx-darwin-arm64.txt", SHA256: sha(lock), Text: lock},
		Imports: []string{"mlx_whisper", "mlx.core"},
		Worker:  []KitFile{{Name: "asr_mlx.py", SHA256: sha(string(py)), Data: py}},
		Model: KitModel{ID: "model:mlx:large-v3-turbo:fp16", Files: []KitDownload{
			{Name: "weights.safetensors", URL: "https://huggingface.co/x/resolve/r/model.safetensors", SHA256: strings.Repeat("2", 64), Size: 5}}},
	}
}

func TestKitValidate(t *testing.T) {
	k := testKit()
	if err := k.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*KitSpec){
		"http url":         func(k *KitSpec) { k.Python.URL = "http://example.org/p" },
		"file url":         func(k *KitSpec) { k.Model.Files[0].URL = "file:///etc/passwd" },
		"user in url":      func(k *KitSpec) { k.Python.URL = "https://u:p@example.org/p" },
		"exe escapes":      func(k *KitSpec) { k.Python.Exe = "../../bin/sh" },
		"lock hash":        func(k *KitSpec) { k.Lock.Text += "b==2\n" },
		"worker hash":      func(k *KitSpec) { k.Worker[0].Data = []byte("import os; os.system('x')") },
		"worker name":      func(k *KitSpec) { k.Worker[0].Name = "../asr_mlx.py" },
		"no engine script": func(k *KitSpec) { k.Worker[0].Name = "other.py"; k.Worker[0].SHA256 = sha(string(k.Worker[0].Data)) },
		"model name":       func(k *KitSpec) { k.Model.Files[0].Name = "../../.ssh/authorized_keys" },
		"model id":         func(k *KitSpec) { k.Model.ID = "x" },
		"import":           func(k *KitSpec) { k.Imports = []string{"os; os.system('x')"} },
		"engine":           func(k *KitSpec) { k.Engine = "sh" },
	} {
		k := testKit()
		k.Worker = append([]KitFile(nil), k.Worker...)
		k.Model.Files = append([]KitDownload(nil), k.Model.Files...)
		mut(&k)
		if err := k.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// http to a loopback host is allowed (the tests' sources).
	k = testKit()
	k.Python.URL = "http://127.0.0.1:8080/p"
	if err := k.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestKitHash(t *testing.T) {
	a, b := testKit(), testKit()
	if a.Hash() != b.Hash() || !KitPattern.MatchString(a.Hash()) {
		t.Fatal("hash not stable")
	}
	b.Model.Files[0].SHA256 = strings.Repeat("3", 64)
	if a.Hash() == b.Hash() {
		t.Fatal("the model's hash is not part of the kit hash")
	}
	c := testKit()
	c.Python.URL = "https://mirror.example/p.tar.gz" // where it comes from does not matter, what it is does
	if a.Hash() != c.Hash() {
		t.Fatal("the source URL changed the kit hash")
	}
	if a.ModelKey() != "model__mlx__large-v3-turbo__fp16" || len(a.VenvKey()) != 16 || len(a.WorkerKey()) != 16 {
		t.Fatal("keys")
	}
}
