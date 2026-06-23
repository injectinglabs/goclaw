package tools

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// UseSkillTool gates skill activation by the caller's grants/visibility, then
// emits a tracing event and returns the SKILL.md path for the model to read.
// Without the gate, members of a shared tenant could "activate" another
// member's private skill and the model would hallucinate its contents.
type UseSkillTool struct {
	access store.SkillAccessStore // nil in single-user editions; gate is skipped
}

func NewUseSkillTool() *UseSkillTool { return &UseSkillTool{} }

// SetSkillAccessStore enables the multi-tenant access check. Mirrors the
// skill_search wiring in cmd/gateway_setup.go.
func (t *UseSkillTool) SetSkillAccessStore(sas store.SkillAccessStore) {
	t.access = sas
}

func (t *UseSkillTool) Name() string { return "use_skill" }

func (t *UseSkillTool) Description() string {
	return "Activate a skill. Call this before read_file to signal skill usage for tracing and observability."
}

func (t *UseSkillTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{
				"type":        "string",
				"description": "Skill name or slug to activate",
			},
			"params": map[string]any{
				"type":        "object",
				"description": "Optional skill-specific parameters",
			},
		},
		"required": []string{"name"},
	}
}

func (t *UseSkillTool) Execute(ctx context.Context, args map[string]any) *Result {
	name, _ := args["name"].(string)
	name = strings.TrimSpace(name)
	if name == "" {
		return ErrorResult("name parameter is required")
	}

	if t.access == nil {
		// Single-user / desktop edition — no multi-tenant boundary to enforce.
		slog.Info("skill.activated", "skill", name, "gated", false)
		if lock := SkillToolLockFromCtx(ctx); lock != nil {
			lock.Mark(name)
		}
		return NewResult(activationMessage(name, ""))
	}

	userID := store.UserIDFromContext(ctx)
	accessible, err := t.access.ListAccessible(ctx, store.AgentIDFromContext(ctx), userID)
	if err != nil {
		slog.Warn("use_skill: access lookup failed", "skill", name, "error", err)
		return ErrorResult("skill access lookup failed; try again")
	}

	match := findSkill(accessible, name)
	if match == nil {
		// Same response shape as "skill does not exist" so callers can't
		// probe for the existence of skills they shouldn't see.
		slog.Info("security.use_skill.denied", "skill", name, "user", userID)
		return ErrorResult(fmt.Sprintf("skill %q not found", name))
	}

	slog.Info("skill.activated", "skill", match.Slug, "user", userID, "gated", true)
	if lock := SkillToolLockFromCtx(ctx); lock != nil {
		lock.Mark(match.Slug)
	}
	return NewResult(activationMessage(match.Slug, match.Path))
}

// findSkill matches by slug then by display name, case-insensitively.
func findSkill(skills []store.SkillInfo, query string) *store.SkillInfo {
	for i := range skills {
		if strings.EqualFold(skills[i].Slug, query) || strings.EqualFold(skills[i].Name, query) {
			return &skills[i]
		}
	}
	return nil
}

// activationMessage builds the user-facing tool result. When path is known,
// pointing the model at it prevents probing the filesystem with list_files /
// bash for SKILL.md candidates (which is what +31's run did and what tipped
// us off to this bug).
func activationMessage(slug, path string) string {
	if path != "" {
		return fmt.Sprintf("Skill %q activated. Read its SKILL.md with read_file(%q).", slug, path)
	}
	return fmt.Sprintf("Skill %q activated. Proceed to read the skill's SKILL.md with read_file.", slug)
}
