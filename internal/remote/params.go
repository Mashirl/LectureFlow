// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package remote

// The worker parameters of a remote run (plan §5): the job.json the local
// constructor builds (stages.asrCallAt) without its paths. The agent adds
// its own paths (AgentPathKeys) and nothing else; every other key must be
// one of ParamKeys with a value of the right type, so a remote never gets
// a path or an option this build does not know.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// PathKeys are the job.json keys the controller removes (and the agent
// sets), per engine; checkpoint.path too.
var PathKeys = map[string][]string{
	EngineMLX:           {"audio", "model_dir"},
	EngineFasterWhisper: {"audio", "model"},
}

type paramSpec struct {
	kind    string // string | bool | int | checkpoint
	re      *regexp.Regexp
	enum    []string
	max     int // string bytes / int value
	minimum int
}

var (
	langRe     = regexp.MustCompile(`^[A-Za-z-]{0,32}$`)
	nameRe     = regexp.MustCompile(`^[A-Za-z0-9._-]{0,64}$`)
	deviceIDRe = regexp.MustCompile(`^[a-z]{2,6}:[a-z0-9-]{1,32}$`)
	hashRe     = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ParamKeys are the allowed keys per engine.
var ParamKeys = map[string]map[string]paramSpec{
	EngineMLX: {
		"model":           {kind: "string", re: nameRe},
		"precision":       {kind: "string", enum: []string{"fp16", "fp32", "int8", "int4"}},
		"device":          {kind: "string", enum: []string{"mlx:gpu"}},
		"language":        {kind: "string", re: langRe},
		"initial_prompt":  {kind: "string", max: 16 << 10},
		"word_timestamps": {kind: "bool"},
		"checkpoint":      {kind: "checkpoint"},
	},
	EngineFasterWhisper: {
		"compute_type":   {kind: "string", enum: []string{"float16", "int8_float16", "int8", "float32", "int8_float32"}},
		"device":         {kind: "string", enum: []string{"cuda", "cpu"}},
		"language":       {kind: "string", re: langRe},
		"initial_prompt": {kind: "string", max: 16 << 10},
		"cpu_threads":    {kind: "int", max: 256},
		"timing":         {kind: "bool"},
		"redecode":       {kind: "bool"},
		"checkpoint":     {kind: "checkpoint"},
	},
}

var checkpointKeys = map[string]paramSpec{
	"job_id":     {kind: "string", re: JobIDPattern},
	"input_hash": {kind: "string", re: hashRe},
	"device_id":  {kind: "string", re: deviceIDRe},
	"model":      {kind: "string", re: nameRe},
	"precision":  {kind: "string", re: nameRe},
	"chunk_s":    {kind: "int", max: 3600},
}

// CheckParams validates the parameters of a run and returns them decoded.
func CheckParams(engine string, raw json.RawMessage) (map[string]any, error) {
	spec, ok := ParamKeys[engine]
	if !ok {
		return nil, errors.New("bad engine")
	}
	if len(raw) == 0 {
		return nil, errors.New("params missing")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil || m == nil {
		return nil, errors.New("params are not an object")
	}
	if err := checkObject(m, spec, "params"); err != nil {
		return nil, err
	}
	return m, nil
}

func checkObject(m map[string]any, spec map[string]paramSpec, where string) error {
	for k, v := range m {
		s, ok := spec[k]
		if !ok {
			return fmt.Errorf("%s: unknown key %q", where, k)
		}
		if err := checkValue(v, s); err != nil {
			return fmt.Errorf("%s.%s: %w", where, k, err)
		}
	}
	return nil
}

func checkValue(v any, s paramSpec) error {
	switch s.kind {
	case "string":
		str, ok := v.(string)
		if !ok {
			return errors.New("not a string")
		}
		if !utf8.ValidString(str) || strings.ContainsRune(str, 0) {
			return errors.New("bad text")
		}
		if s.max > 0 && len(str) > s.max {
			return errors.New("too long")
		}
		if s.re != nil && !s.re.MatchString(str) {
			return errors.New("bad value")
		}
		if s.enum != nil && !slices.Contains(s.enum, str) {
			return errors.New("unknown value")
		}
	case "bool":
		if _, ok := v.(bool); !ok {
			return errors.New("not a bool")
		}
	case "int":
		n, ok := v.(json.Number)
		if !ok {
			return errors.New("not a number")
		}
		i, err := n.Int64()
		if err != nil || i < int64(s.minimum) || i > int64(s.max) {
			return errors.New("out of range")
		}
	case "checkpoint":
		obj, ok := v.(map[string]any)
		if !ok {
			return errors.New("not an object")
		}
		return checkObject(obj, checkpointKeys, "checkpoint")
	}
	return nil
}

// StripPaths removes the path keys of engine from job (a job.json built by
// the local constructor, as a map) and checkpoint.path, in place.
func StripPaths(engine string, job map[string]any) {
	for _, k := range PathKeys[engine] {
		delete(job, k)
	}
	if ck, ok := job["checkpoint"].(map[string]any); ok {
		delete(ck, "path")
	}
}
