package tools

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// stubAccess implements store.SkillAccessStore for the use_skill gate tests.
type stubAccess struct {
	skills []store.SkillInfo
	err    error
}

func (s *stubAccess) ListAccessible(_ context.Context, _ uuid.UUID, _ string) ([]store.SkillInfo, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.skills, nil
}

func TestUseSkill_NoAccessStore_LegacyActivates(t *testing.T) {
	t.Parallel()
	tool := NewUseSkillTool()
	r := tool.Execute(context.Background(), map[string]any{"name": "anything"})
	if r.IsError {
		t.Fatalf("legacy edition must activate; got error %q", r.ForLLM)
	}
	if !strings.Contains(r.ForLLM, `"anything" activated`) {
		t.Fatalf("expected activation message; got %q", r.ForLLM)
	}
}

func TestUseSkill_Accessible_ReturnsPath(t *testing.T) {
	t.Parallel()
	tool := NewUseSkillTool()
	tool.SetSkillAccessStore(&stubAccess{skills: []store.SkillInfo{
		{Slug: "algorithmic-art", Name: "Algorithmic Art", Path: "/skills/algorithmic-art/1/SKILL.md"},
	}})

	r := tool.Execute(context.Background(), map[string]any{"name": "algorithmic-art"})
	if r.IsError {
		t.Fatalf("expected success; got error %q", r.ForLLM)
	}
	if !strings.Contains(r.ForLLM, "/skills/algorithmic-art/1/SKILL.md") {
		t.Fatalf("expected path in result; got %q", r.ForLLM)
	}
}

func TestUseSkill_NotAccessible_DeniedAsNotFound(t *testing.T) {
	t.Parallel()
	// The skill exists in the tenant but is not in the caller's accessible
	// set — emulates a tenant member trying to activate another user's
	// private skill. Response must be indistinguishable from "does not
	// exist" so the gate doesn't leak the skill's existence.
	tool := NewUseSkillTool()
	tool.SetSkillAccessStore(&stubAccess{skills: nil})

	r := tool.Execute(context.Background(), map[string]any{"name": "algorithmic-art"})
	if !r.IsError {
		t.Fatalf("expected error result; got success %q", r.ForLLM)
	}
	if !strings.Contains(r.ForLLM, `not found`) {
		t.Fatalf("expected 'not found' phrasing; got %q", r.ForLLM)
	}
}

func TestUseSkill_MatchByName_CaseInsensitive(t *testing.T) {
	t.Parallel()
	tool := NewUseSkillTool()
	tool.SetSkillAccessStore(&stubAccess{skills: []store.SkillInfo{
		{Slug: "algorithmic-art", Name: "Algorithmic Art", Path: "/p/SKILL.md"},
	}})
	r := tool.Execute(context.Background(), map[string]any{"name": "Algorithmic Art"})
	if r.IsError {
		t.Fatalf("expected match by display name; got %q", r.ForLLM)
	}
}

func TestUseSkill_LookupError_FailClosed(t *testing.T) {
	t.Parallel()
	tool := NewUseSkillTool()
	tool.SetSkillAccessStore(&stubAccess{err: errors.New("db down")})
	r := tool.Execute(context.Background(), map[string]any{"name": "algorithmic-art"})
	if !r.IsError {
		t.Fatalf("DB error must fail closed; got success %q", r.ForLLM)
	}
}

func TestUseSkill_EmptyName_Rejected(t *testing.T) {
	t.Parallel()
	tool := NewUseSkillTool()
	r := tool.Execute(context.Background(), map[string]any{"name": "   "})
	if !r.IsError {
		t.Fatalf("empty name must be rejected")
	}
}
