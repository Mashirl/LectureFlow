// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Package sysprobe reads a machine's resources for lf-agent (ssh-mvp; plan
// §6: "远端资源采样", not telemetry): nvidia-smi on NVIDIA machines, /proc
// on Linux, sysctl / vm_stat / ioreg / ps on macOS. A leaf package
// (standard library only), so the agent stays free of the application's
// packages. The nvidia-smi query is the same as internal/resources'
// (QueryArgs, ParseQuery; a test keeps the two parsers in step).
package sysprobe

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// QueryArgs are the nvidia-smi arguments of a sample (internal/resources.QueryArgs).
var QueryArgs = []string{
	"--query-gpu=name,utilization.gpu,memory.used,memory.total,temperature.gpu,power.draw,power.limit",
	"--format=csv,noheader,nounits",
}

// MachineArgs ask nvidia-smi for the identity of the first GPU.
var MachineArgs = []string{
	"--query-gpu=name,memory.total,driver_version,compute_cap,count",
	"--format=csv,noheader,nounits",
}

const queryColumns = 7

// cmdTimeout bounds one external command.
const cmdTimeout = 5 * time.Second

// GPUSample is one parsed nvidia-smi reading of the first GPU; nil = not
// reported.
type GPUSample struct {
	Name        string
	UtilPct     *int
	MemUsedMB   *int
	MemTotalMB  *int
	TempC       *int
	PowerW      *float64
	PowerLimitW *float64
}

// ParseQuery parses the output of nvidia-smi with QueryArgs (the first
// non-empty line). "[N/A]"-like columns become nil; other unexpected
// output is an error.
func ParseQuery(out string) (GPUSample, error) {
	line := firstLine(out)
	if line == "" {
		return GPUSample{}, errors.New("nvidia-smi: empty output")
	}
	parts := strings.Split(line, ",")
	if len(parts) != queryColumns {
		return GPUSample{}, fmt.Errorf("nvidia-smi: unexpected output")
	}
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	s := GPUSample{Name: parts[0]}
	var err error
	for i, dst := range []**int{&s.UtilPct, &s.MemUsedMB, &s.MemTotalMB, &s.TempC} {
		if *dst, err = parseInt(parts[1+i]); err != nil {
			return GPUSample{}, fmt.Errorf("nvidia-smi: unexpected output")
		}
	}
	if s.PowerW, err = parseFloat(parts[5]); err != nil {
		return GPUSample{}, fmt.Errorf("nvidia-smi: unexpected output")
	}
	if s.PowerLimitW, err = parseFloat(parts[6]); err != nil {
		return GPUSample{}, fmt.Errorf("nvidia-smi: unexpected output")
	}
	return s, nil
}

// GPUMachine is the identity of the first GPU.
type GPUMachine struct {
	Name       string
	MemTotalMB *int
	Driver     string
	ComputeCap string
	Count      *int
}

// ParseMachine parses the output of nvidia-smi with MachineArgs.
func ParseMachine(out string) (GPUMachine, error) {
	line := firstLine(out)
	parts := strings.Split(line, ",")
	if line == "" || len(parts) != 5 {
		return GPUMachine{}, errors.New("nvidia-smi: unexpected output")
	}
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	m := GPUMachine{Name: parts[0]}
	var err error
	if m.MemTotalMB, err = parseInt(parts[1]); err != nil {
		return GPUMachine{}, err
	}
	if !notReported(parts[2]) {
		m.Driver = parts[2]
	}
	if !notReported(parts[3]) {
		m.ComputeCap = parts[3]
	}
	if m.Count, err = parseInt(parts[4]); err != nil {
		return GPUMachine{}, err
	}
	return m, nil
}

// notReported matches nvidia-smi's placeholders for values it cannot read.
func notReported(v string) bool {
	return v == "" || v == "N/A" || (strings.HasPrefix(v, "[") && strings.HasSuffix(v, "]"))
}

func parseFloat(v string) (*float64, error) {
	if notReported(v) {
		return nil, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return nil, fmt.Errorf("not a number: %q", v)
	}
	return &f, nil
}

func parseInt(v string) (*int, error) {
	if notReported(v) {
		return nil, nil
	}
	if n, err := strconv.Atoi(v); err == nil {
		return &n, nil
	}
	f, err := parseFloat(v)
	if err != nil {
		return nil, err
	}
	n := int(math.Round(*f))
	return &n, nil
}

func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}

// run runs a command with the timeout and returns its stdout.
func run(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, cmdTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	return string(out), err
}

// NvidiaSMI finds nvidia-smi ("" when there is none).
func NvidiaSMI() string {
	for _, p := range []string{"nvidia-smi", "/usr/bin/nvidia-smi", "/usr/local/nvidia/bin/nvidia-smi"} {
		if q, err := exec.LookPath(p); err == nil {
			return q
		}
	}
	return ""
}
