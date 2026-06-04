package agent

import (
	"context"

	"github.com/nextlevelbuilder/goclaw/internal/skills"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// Hybrid skill thresholds: when skill count and total token estimate are below
// these limits, inline all skills as XML in the system prompt (like TS).
// Above these limits, only include skill_search instructions.
const (
	skillInlineMaxCount  = 60   // max skills to inline
	skillInlineMaxTokens = 3000 // max estimated tokens for skill descriptions
	skillSummaryDescCap  = 200  // matches skills.skillDescMaxLen truncation
)

// resolveSkillsSummary dynamically builds the skills summary for the system prompt.
// Called per-message so it picks up hot-reloaded / newly-uploaded skills.
// Returns the <available_skills> XML, or "" to fall back to skill_search-only mode.
//
// When a SkillAccessStore is wired (multi-tenant / PostgreSQL), the summary is
// sourced from the DB via ListAccessible — the filesystem loader never sees
// DB-stored or per-tenant skills, so loader-based summaries are empty for real
// tenants. Falls back to the loader for single-user / desktop editions.
func (l *Loop) resolveSkillsSummary(ctx context.Context, skillFilter []string) string {
	if l.skillAccess != nil {
		return l.resolveSkillsSummaryFromAccessStore(ctx, skillFilter)
	}

	if l.skillsLoader == nil {
		return ""
	}

	// Per-request skill filter overrides agent-level allowList.
	allowList := l.skillAllowList
	if skillFilter != nil {
		allowList = skillFilter
	}

	filtered := l.skillsLoader.FilterSkills(ctx, allowList)
	return summaryFromInfosCapped(filtered)
}

// resolveSkillsSummaryFromAccessStore builds the summary from the tenant-scoped
// DB skill set (ListAccessible), applying the same inline thresholds as the
// loader path.
func (l *Loop) resolveSkillsSummaryFromAccessStore(ctx context.Context, skillFilter []string) string {
	accessible, err := l.skillAccess.ListAccessible(ctx, l.agentUUID, store.UserIDFromContext(ctx))
	if err != nil || len(accessible) == 0 {
		return ""
	}

	var allowed map[string]bool
	if skillFilter != nil {
		allowed = make(map[string]bool, len(skillFilter))
		for _, s := range skillFilter {
			allowed[s] = true
		}
	}

	infos := make([]skills.Info, 0, len(accessible))
	for _, sk := range accessible {
		// Don't advertise archived / non-active skills (e.g. missing deps).
		if sk.Status != "" && sk.Status != "active" {
			continue
		}
		if allowed != nil && !allowed[sk.Slug] {
			continue
		}
		infos = append(infos, storeSkillInfoToInfo(sk))
	}
	return summaryFromInfosCapped(infos)
}

// resolvePinnedSkillsSummary builds XML for pinned skills only (always inline).
func (l *Loop) resolvePinnedSkillsSummary(ctx context.Context) string {
	if len(l.pinnedSkills) == 0 {
		return ""
	}

	if l.skillAccess != nil {
		accessible, err := l.skillAccess.ListAccessible(ctx, l.agentUUID, store.UserIDFromContext(ctx))
		if err != nil {
			return ""
		}
		pinned := make(map[string]bool, len(l.pinnedSkills))
		for _, n := range l.pinnedSkills {
			pinned[n] = true
		}
		var infos []skills.Info
		for _, sk := range accessible {
			if pinned[sk.Slug] || pinned[sk.Name] {
				infos = append(infos, storeSkillInfoToInfo(sk))
			}
		}
		return skills.BuildSummaryFromInfos(infos)
	}

	if l.skillsLoader == nil {
		return ""
	}
	return l.skillsLoader.BuildPinnedSummary(ctx, l.pinnedSkills)
}

// summaryFromInfosCapped renders the inline summary when the set is within the
// hybrid inline thresholds; otherwise returns "" so the agent uses skill_search.
func summaryFromInfosCapped(infos []skills.Info) string {
	if len(infos) == 0 || len(infos) > skillInlineMaxCount {
		return ""
	}
	totalChars := 0
	for _, s := range infos {
		descLen := min(len(s.Description), skillSummaryDescCap)
		totalChars += len(s.Name) + descLen + 10 // +10 for XML tag overhead
	}
	if totalChars/4 > skillInlineMaxTokens {
		return ""
	}
	return skills.BuildSummaryFromInfos(infos)
}

// storeSkillInfoToInfo adapts a DB SkillInfo to the loader's Info for summary
// rendering. Name + Description feed the prompt; Path is the SKILL.md location.
func storeSkillInfoToInfo(s store.SkillInfo) skills.Info {
	return skills.Info{
		Name:        s.Name,
		Slug:        s.Slug,
		Path:        s.Path,
		BaseDir:     s.BaseDir,
		Source:      s.Source,
		Description: s.Description,
	}
}
