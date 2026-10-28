// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"lectureflow/internal/remote"
	"lectureflow/internal/sysprobe"
)

// probe reports the machine (display and the controller's model choice):
// platform, OS, CPUs, memory, free disk, the GPU and the installed kits.
// No host or user name.
func (e *env) probe() int {
	ctx := context.Background()
	m := sysprobe.NewSampler().Machine(ctx)
	ev := map[string]any{
		"type": remote.EvProbe, "platform": Platform(), "os_version": remote.CleanText(m.OS, remote.MaxProbeString),
		"cpus": m.VCPUs, "mem_total_mb": m.MemTotalMB, "disk_free_mb": -1, "vram_total_mb": m.VRAMTotalMB,
		"metal": m.Metal,
	}
	if free, ok := diskFreeMB(e.dirs.Root); ok {
		ev["disk_free_mb"] = free
	}
	if remote.GPUNamePattern.MatchString(m.GPUName) {
		ev["gpu_name"] = m.GPUName
	}
	if remote.DriverPattern.MatchString(m.Driver) {
		ev["driver"] = m.Driver
	}
	if remote.VersionPattern.MatchString(m.CUDA) {
		ev["cuda"] = m.CUDA
	}
	if kits := e.kits(); len(kits) > 0 {
		ev["kits"] = strings.Join(kits, ",")
	}
	e.out.emit(ev)
	return 0
}

// kits lists the installed kit hashes.
func (e *env) kits() []string {
	ents, err := os.ReadDir(filepath.Join(e.dirs.Root, dirKits))
	if err != nil {
		return nil
	}
	var out []string
	for _, d := range ents {
		h := strings.TrimSuffix(d.Name(), ".json")
		if d.Type().IsRegular() && remote.KitPattern.MatchString(h) && len(out) < 64 {
			out = append(out, h)
		}
	}
	return out
}

// uninstall removes the agent's directories (plan §4: "移除服务器时
// lf-agent uninstall 删整个目录"), the models too unless keepModels.
func (e *env) uninstall(keepModels bool) int {
	failed := false
	if err := removeTree(e.dirs.Jobs); err != nil {
		failed = true
	}
	// The job directory's parent on macOS / Linux is the agent's own
	// cache directory ("…/LectureFlow Agent", "…/lectureflow-agent").
	if parent := filepath.Dir(e.dirs.Jobs); parent != e.dirs.Root && isAgentDir(parent) {
		if err := removeTree(parent); err != nil {
			failed = true
		}
	}
	if keepModels {
		ents, _ := os.ReadDir(e.dirs.Root)
		for _, d := range ents {
			if d.Name() == dirModels {
				continue
			}
			if err := removeAllInside(e.dirs.Root, filepath.Join(e.dirs.Root, d.Name())); err != nil {
				failed = true
			}
		}
	} else if err := removeTree(e.dirs.Root); err != nil {
		failed = true
	}
	if failed {
		e.fail("", remote.AgentInternal)
		return 1
	}
	e.out.emit(map[string]any{"type": remote.EvOK})
	return 0
}

// isAgentDir: a directory the agent created (its name).
func isAgentDir(p string) bool {
	b := filepath.Base(p)
	return b == "LectureFlow Agent" || b == "lectureflow-agent" || b == "LectureFlowAgent"
}

// removeTree removes one of the agent's own directories (checked by name).
func removeTree(p string) error {
	if !isAgentDir(p) && filepath.Base(p) != "jobs" {
		return errUnsafe
	}
	fi, err := os.Lstat(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return os.Remove(p) // a link where our directory should be: remove the link only
	}
	return os.RemoveAll(p)
}
