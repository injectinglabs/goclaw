package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/skills"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

type stubSkillAccess struct{ skills []store.SkillInfo }

func (s stubSkillAccess) ListAccessible(_ context.Context, _ uuid.UUID, _ string) ([]store.SkillInfo, error) {
	return s.skills, nil
}

// Regression for the tenant-scoped skill_search bug: a skill that lives only in
// the access store (DB / per-tenant skills-store) — NOT on the loader's
// filesystem index — must still be discoverable. Before the fix, skill_search
// searched only the shared filesystem index built from a tenant-less startup
// context, so DB-stored and per-tenant skills were never found (search returned
// "No skills found" for every real tenant skill).
func TestSkillSearch_FindsAccessStoreSkillNotInLoaderIndex(t *testing.T) {
	// Empty loader dirs → the shared filesystem index has zero skills, so a hit
	// can only come from the access-store path.
	dir := t.TempDir()
	loader := skills.NewLoader(dir, dir, dir)
	tool := NewSkillSearchTool(loader)
	tool.SetSkillAccessStore(stubSkillAccess{skills: []store.SkillInfo{{
		Name:        "near",
		Slug:        "near",
		Description: "near token price",
		Path:        dir + "/near/1/SKILL.md",
		BaseDir:     dir + "/near/1",
		Source:      "managed",
		Status:      "active",
		Visibility:  "public",
	}}})

	ctx := store.WithUserID(store.WithAgentID(context.Background(), uuid.New()), "user-1")
	res := tool.Execute(ctx, map[string]any{"query": "near"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "near") {
		t.Fatalf("expected the per-tenant 'near' skill in results, got: %s", res.ForLLM)
	}
}

// Sanity: with no access store wired (single-user / desktop), an empty loader
// index returns no results rather than erroring.
func TestSkillSearch_NoAccessStore_EmptyLoader(t *testing.T) {
	dir := t.TempDir()
	tool := NewSkillSearchTool(skills.NewLoader(dir, dir, dir))
	res := tool.Execute(context.Background(), map[string]any{"query": "near"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "No skills found") {
		t.Fatalf("expected no-results message, got: %s", res.ForLLM)
	}
}
