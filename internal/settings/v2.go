// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package settings

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// v2 (SPEC-v2.md) settings functions. Signatures FROZEN by the lead; bodies
// by llm-builder (.claude/v2-brief.md, ruling 12).

// DefaultJanitorModel is the glossary_janitor model of appendix D1 when the
// Anthropic provider has no probed model list.
const DefaultJanitorModel = "claude-opus-5-5"

// defaultRoles are the roles appendix D1 can fill, in the order they are
// filled. A role is only filled while model.Roles lists it (phase 2 added
// chapters and organizer, lead ruling 11).
var defaultRoles = []string{model.RoleGlossaryJanitor, model.RoleChapters, model.RoleOrganizer}

// ApplyRoleDefaults writes the appendix D1 defaults of the roles that are
// not configured yet and records kv v2.role_default:{role} for each role it
// filled. It never changes a configured role. It returns the roles it filled.
// Called once at startup, after migrations.
//
// glossary_janitor: the first Anthropic provider (created order) with a
// key, using the newest Opus model of its probed model list, else (no probe
// result) DefaultJanitorModel. Providers whose probed list has no Opus model,
// and DeepSeek's Anthropic-compatible endpoint (it serves claude-opus-* names
// with a DeepSeek model), are skipped. No such provider: nothing is written
// and nothing is recorded, so the next start decides again.
// chapters, organizer: the provider and model of the summary role, when it
// is configured.
func (s *Service) ApplyRoleDefaults(ctx context.Context) ([]string, error) {
	type fill struct{ role, provider, model string }
	var filled []fill
	s.wmu.Lock()
	err := s.st.Tx(ctx, func(tx db.Store) error {
		filled = nil
		rows, err := tx.ListRoleSettings(ctx)
		if err != nil {
			return err
		}
		current := make(map[string]model.RoleSetting, len(rows))
		for _, r := range rows {
			current[r.Role] = r
		}
		provs, err := tx.ListProviders(ctx)
		if err != nil {
			return err
		}
		for _, role := range defaultRoles {
			if !validRole(role) || roleConfigured(current[role], provs) {
				continue
			}
			p, m, ok, err := roleDefault(ctx, tx, role, current, provs)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			row := model.RoleSetting{Role: role, ProviderID: &p.ID, Model: &m, Params: "{}"}
			if err := tx.UpsertRoleSetting(ctx, row); err != nil {
				return err
			}
			if err := tx.SetKV(ctx, model.KVRoleDefaultPrefix+role, "1"); err != nil {
				return err
			}
			current[role] = row
			filled = append(filled, fill{role, p.Name, m})
		}
		return nil
	})
	if err == nil && len(filled) > 0 {
		err = s.Load(ctx)
	}
	s.wmu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("写入默认的角色分配失败: %w", err)
	}
	roles := make([]string, len(filled))
	for i, f := range filled {
		roles[i] = f.role
		slog.Info("role filled with its default (SPEC-v2 appendix D1)", "role", f.role, "provider", f.provider, "model", f.model)
	}
	if len(filled) > 0 {
		s.changed()
	}
	return roles, nil
}

// RoleDefault reports whether the role's current value was filled in by
// ApplyRoleDefaults and not changed by the user since (the settings page
// shows a "默认" tag). SetRole clears the mark.
func (s *Service) RoleDefault(role string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.defaults[role]
}

// RoleHint is the explanation line of a role on the settings page
// (api.RoleView.Hint); "" for roles without one.
func RoleHint(role string) string {
	switch role {
	case model.RoleGlossaryJanitor:
		return "建议选择与术语提取不同的模型，例如 Claude Opus，以便互相纠错。" // G3.1, verbatim
	}
	return ""
}

// IsDeepSeek reports whether a provider is DeepSeek's API: the base_url
// host is deepseek.com or one of its subdomains, or the provider uses the
// DeepSeek balance API. The llm client switches thinking off for it
// (appendix D2).
func IsDeepSeek(baseURL, balanceAPI string) bool {
	if balanceAPI == model.BalanceDeepSeek {
		return true
	}
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return false
	}
	h := strings.ToLower(u.Hostname())
	return h == "deepseek.com" || strings.HasSuffix(h, ".deepseek.com")
}

// markRoles are the roles that can carry a default mark.
func markRoles() []string {
	out := append([]string(nil), model.Roles...)
	for _, r := range defaultRoles {
		if !validRole(r) {
			out = append(out, r)
		}
	}
	return out
}

// loadDefaultMarks reads the appendix D1 marks (kv v2.role_default:{role}).
func loadDefaultMarks(ctx context.Context, st db.Store) (map[string]bool, error) {
	out := map[string]bool{}
	for _, role := range markRoles() {
		v, ok, err := st.GetKV(ctx, model.KVRoleDefaultPrefix+role)
		if err != nil {
			return nil, err
		}
		if ok && strings.TrimSpace(v) != "" {
			out[role] = true
		}
	}
	return out, nil
}

// clearDefaultMark removes the role's mark (the Store has no kv delete: an
// empty value reads as "no mark").
func clearDefaultMark(ctx context.Context, tx db.Store, role string) error {
	v, ok, err := tx.GetKV(ctx, model.KVRoleDefaultPrefix+role)
	if err != nil || !ok || v == "" {
		return err
	}
	return tx.SetKV(ctx, model.KVRoleDefaultPrefix+role, "")
}

// roleConfigured: the row names a provider that exists and a model.
func roleConfigured(r model.RoleSetting, provs []model.Provider) bool {
	if r.ProviderID == nil || *r.ProviderID == "" || r.Model == nil || strings.TrimSpace(*r.Model) == "" {
		return false
	}
	_, ok := findProvider(provs, *r.ProviderID)
	return ok
}

func findProvider(provs []model.Provider, id string) (model.Provider, bool) {
	for _, p := range provs {
		if p.ID == id {
			return p, true
		}
	}
	return model.Provider{}, false
}

// roleDefault computes the appendix D1 default of one role.
func roleDefault(ctx context.Context, tx db.Store, role string, current map[string]model.RoleSetting, provs []model.Provider) (model.Provider, string, bool, error) {
	switch role {
	case model.RoleGlossaryJanitor:
		return janitorDefault(ctx, tx, provs)
	case model.RoleChapters, model.RoleOrganizer:
		sum := current[model.RoleSummary]
		if !roleConfigured(sum, provs) {
			return model.Provider{}, "", false, nil
		}
		p, _ := findProvider(provs, *sum.ProviderID)
		return p, strings.TrimSpace(*sum.Model), true, nil
	}
	return model.Provider{}, "", false, nil
}

// janitorDefault picks the glossary_janitor default: an Anthropic provider
// whose probed list has an Opus model wins over one that was never probed
// (that one gets DefaultJanitorModel); among equals the first created wins.
func janitorDefault(ctx context.Context, tx db.Store, provs []model.Provider) (model.Provider, string, bool, error) {
	var unprobed *model.Provider
	for _, p := range provs {
		if p.Type != model.ProviderAnthropic || strings.TrimSpace(p.APIKey) == "" || IsDeepSeek(p.BaseURL, p.BalanceAPI) {
			continue
		}
		pm, ok, err := probedModels(ctx, tx, p.ID)
		if errors.Is(err, errProbeCache) {
			ok, err = false, nil // a broken cache is no probe result
		}
		if err != nil {
			return model.Provider{}, "", false, err
		}
		if ok && len(pm.Models) > 0 {
			if m, found := LatestOpus(pm.Models); found {
				return p, m, true, nil
			}
			continue // probed, and this key lists no Opus model
		}
		if unprobed == nil {
			cp := p
			unprobed = &cp
		}
	}
	if unprobed != nil {
		return *unprobed, DefaultJanitorModel, true, nil
	}
	return model.Provider{}, "", false, nil
}

// LatestOpus returns the newest Opus model of a model list, comparing the
// version numbers in the names (claude-opus-5-5 > claude-opus-5 >
// claude-opus-4-8 > claude-3-opus-20240229). Equal versions prefer the
// shorter id (the alias over a dated snapshot).
func LatestOpus(models []string) (string, bool) {
	best, bestV := "", []int(nil)
	found := false
	for _, m := range models {
		m = strings.TrimSpace(m)
		v, ok := opusVersion(m)
		if !ok {
			continue
		}
		if !found {
			best, bestV, found = m, v, true
			continue
		}
		switch c := compareVersions(v, bestV); {
		case c > 0:
			best, bestV = m, v
		case c == 0 && (len(m) < len(best) || (len(m) == len(best) && m > best)):
			best = m
		}
	}
	return best, found
}

// opusVersion parses the version of an Opus model id: the numbers after
// "opus" (claude-opus-4-1-20250805 → [4 1]; a dated snapshot or another
// suffix ends the version), or, for the old naming, the numbers before it
// (claude-3-opus-20240229 → [3]). A Vertex "@date" suffix is ignored.
func opusVersion(id string) ([]int, bool) {
	s := strings.ToLower(id)
	if i := strings.IndexByte(s, '@'); i >= 0 {
		s = s[:i]
	}
	toks := strings.Split(s, "-")
	at := -1
	for i, t := range toks {
		if t == "opus" {
			at = i
			break
		}
	}
	if at < 0 {
		return nil, false
	}
	num := func(t string) (int, bool) {
		if len(t) == 0 || len(t) > 2 { // 8-digit dates are not versions
			return 0, false
		}
		n, err := strconv.Atoi(t)
		return n, err == nil
	}
	var v []int
	for _, t := range toks[at+1:] {
		n, ok := num(t)
		if !ok {
			break
		}
		v = append(v, n)
	}
	if len(v) == 0 {
		for _, t := range toks[:at] {
			if n, ok := num(t); ok {
				v = append(v, n)
			}
		}
	}
	return v, true
}

// compareVersions compares two version number lists ([5] < [5 5]).
func compareVersions(a, b []int) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			if a[i] > b[i] {
				return 1
			}
			return -1
		}
	}
	switch {
	case len(a) > len(b):
		return 1
	case len(a) < len(b):
		return -1
	}
	return 0
}
