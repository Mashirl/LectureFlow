// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"regexp"
	"strings"

	"lectureflow/internal/model"
	"lectureflow/internal/settings"
)

// Role models missing from their provider's model list (lead, after the
// DeepSeek documentation check of 2026-10-23: the old name deepseek-chat
// is past its retirement date and the provider's probed list no longer
// has it). A role whose model is not in the last list probed for its
// provider gets a warn-level diagnostics row (check.role_model_unlisted,
// one row per provider and model, naming the roles), so a retired name is
// noticed before the whole pipeline fails on it. Nothing is said while a
// provider has no list (never probed, or every probe failed): a failed
// probe keeps the previous list (probeProvider). The settings page marks
// the role rows by the same rule (web/view_settings.js modelListed).

// datedSuffix is the date that dated model ids append to an alias
// ("claude-sonnet-4-5-20250929").
var datedSuffix = regexp.MustCompile(`^[0-9]{6,8}$`)

// modelListed reports whether name is in list: the same id (ignoring
// case), or an alias of a dated id in it: "claude-sonnet-4-5" (or
// "claude-sonnet-4-5-latest") for "claude-sonnet-4-5-20250929". Providers
// answer aliases like that, while their model lists show only the dated
// ids.
func modelListed(name string, list []string) bool {
	m := strings.ToLower(strings.TrimSpace(name))
	base := strings.TrimSuffix(m, "-latest")
	for _, id := range list {
		id = strings.ToLower(id)
		if id == m {
			return true
		}
		if rest, ok := strings.CutPrefix(id, base+"-"); ok && datedSuffix.MatchString(rest) {
			return true
		}
	}
	return false
}

// RoleModelChecks are the diagnostics rows for role models that their
// provider's probed list does not have: one warn row per provider and
// model, in the order of model.Roles, named "role_model:{provider}/{model}",
// detail check.role_model_unlisted {roles (role ids; the front end shows
// its role names), model, provider (name)}, hint
// check.hint_role_model_unlisted {provider}. Roles without a provider or
// model, with a deleted provider (their own error says so) or whose
// provider has no list are skipped. Pure: the API and mockserve pass
// their settings views.
func RoleModelChecks(roles []RoleView, providers []ProviderView) []SystemCheck {
	byID := make(map[string]ProviderView, len(providers))
	for _, p := range providers {
		byID[p.ID] = p
	}
	type key struct{ provider, model string }
	var order []key
	groups := map[key][]string{}
	names := map[key]string{}
	for _, r := range roles {
		if r.ProviderID == nil || r.Model == nil || strings.TrimSpace(*r.Model) == "" {
			continue
		}
		p, ok := byID[*r.ProviderID]
		if !ok || len(p.Models) == 0 || modelListed(*r.Model, p.Models) {
			continue
		}
		k := key{p.ID, strings.TrimSpace(*r.Model)}
		if _, seen := groups[k]; !seen {
			order = append(order, k)
			names[k] = p.Name
		}
		groups[k] = append(groups[k], r.Role)
	}
	out := make([]SystemCheck, 0, len(order))
	for _, k := range order {
		c := SystemCheck{Name: "role_model:" + names[k] + "/" + k.model, Status: statusWarn}
		c.setDetail("check.role_model_unlisted", map[string]any{"roles": groups[k], "model": k.model, "provider": names[k]})
		// the transitional Chinese detail names the roles, not their ids
		labels := make([]string, len(groups[k]))
		for i, r := range groups[k] {
			labels[i] = settings.RoleLabel(r)
		}
		c.Detail = zhText(model.ErrorInfo{Code: c.DetailInfo.Code, Params: map[string]any{"roles": strings.Join(labels, "、"), "model": k.model, "provider": names[k]}})
		c.setHint("check.hint_role_model_unlisted", map[string]any{"provider": names[k]})
		out = append(out, c)
	}
	return out
}

// roleModelChecks computes RoleModelChecks from the current settings (live:
// GET /api/system follows role changes and probes without a recheck).
func (s *Server) roleModelChecks(ctx context.Context) []SystemCheck {
	if s.set == nil {
		return nil
	}
	ps := s.set.Providers()
	views := make([]ProviderView, 0, len(ps))
	for _, p := range ps {
		v := ProviderView{ID: p.ID, Name: p.Name}
		if pm, ok, err := s.set.ProbedModels(ctx, p.ID); err == nil && ok {
			v.Models = pm.Models
		}
		views = append(views, v)
	}
	return RoleModelChecks(s.roleViews(), views)
}

// WithRoleModelChecks inserts the role-model rows before the live disk
// check, which snapshot puts last (they belong with the API checks).
func WithRoleModelChecks(checks, extra []SystemCheck) []SystemCheck {
	if len(extra) == 0 {
		return checks
	}
	out := make([]SystemCheck, 0, len(checks)+len(extra))
	n := len(checks)
	if n > 0 && checks[n-1].Name == "disk" {
		out = append(append(append(out, checks[:n-1]...), extra...), checks[n-1])
		return out
	}
	return append(append(out, checks...), extra...)
}
