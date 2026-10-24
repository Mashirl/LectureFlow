// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package relocate

import "lectureflow/internal/model"

// Error codes (model.CodedError; the HTTP layer maps them to statuses and
// the front end translates errors.<code>).
const (
	CodePathInvalid       = "data_dir.path_invalid"           // {path}: empty, relative or not a directory
	CodeSameDir           = "data_dir.same_dir"               // the target is the current data directory
	CodeTargetNotEmpty    = "data_dir.target_not_empty"       // {path}
	CodeTargetInside      = "data_dir.target_inside_source"   // the target is inside the data directory
	CodeTargetContains    = "data_dir.target_contains_source" // the data directory is inside the target
	CodeTargetNotWritable = "data_dir.target_not_writable"    // {path}
	CodeNoSpace           = "data_dir.no_space"               // {need_bytes, free_bytes}
	CodeCopyFailed        = "data_dir.copy_failed"            // {path}
	CodeVerifyFailed      = "data_dir.verify_failed"          // {what}
	CodeSourceChanged     = "data_dir.source_changed"         // the database changed during the copy
	CodeComponentsFailed  = "data_dir.components_failed"      // fixing the component environments failed
	CodeLocationFailed    = "data_dir.location_failed"        // writing location.json failed
	CodeCancelled         = "data_dir.cancelled"

	CodeNotLectureFlow = "data_import.not_lectureflow" // {path}: no lectureflow.db or tables not recognised
	CodeTooNew         = "data_import.too_new"         // {version, supported}
	CodeNotFresh       = "data_import.not_fresh"       // {courses, lectures}
	CodeImportSameDir  = "data_import.same_dir"        // the source is (inside / contains) the data directory
)

func coded(code string, params map[string]any, err error) error {
	return model.Errorf(code, params, err)
}

func verifyErr(what string, err error) error {
	return coded(CodeVerifyFailed, map[string]any{"what": what}, err)
}
