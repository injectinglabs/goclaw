package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

func baseAgent() *store.AgentData {
	return &store.AgentData{
		BaseModel: store.BaseModel{ID: uuid.New()},
		AgentKey:  "test-agent",
		AgentType: store.AgentTypePredefined,
		Workspace: "/workspace",
	}
}

func TestBuildPreviewPrompt_NilDeps(t *testing.T) {
	r := BuildPreviewPrompt(context.Background(), baseAgent(), PromptFull, "", PreviewDeps{})
	if r.Prompt == "" {
		t.Fatal("expected non-empty prompt with nil deps")
	}
	if !strings.Contains(r.Prompt, "read_file") {
		t.Error("expected fallback tool names in prompt")
	}
	// No tool lister → no tool defs
	if len(r.ToolDefs) != 0 {
		t.Errorf("expected no tool defs with nil ToolLister, got %d", len(r.ToolDefs))
	}
}

func TestBuildPreviewPrompt_SkillsInline(t *testing.T) {
	r := BuildPreviewPrompt(context.Background(), baseAgent(), PromptFull, "", PreviewDeps{
		SkillsLoader: &mockSkillsLoader{
			summary: "<available_skills>\n<skill name=\"git\">Git operations</skill>\n</available_skills>",
		},
	})
	if !strings.Contains(r.Prompt, "<available_skills>") {
		t.Error("expected skills XML inlined in prompt")
	}
}

func TestBuildPreviewPrompt_SkillsSearchMode(t *testing.T) {
	bigSummary := strings.Repeat("x", 10000)
	r := BuildPreviewPrompt(context.Background(), baseAgent(), PromptFull, "", PreviewDeps{
		SkillsLoader: &mockSkillsLoader{summary: bigSummary},
	})
	if strings.Contains(r.Prompt, bigSummary) {
		t.Error("expected large summary to be excluded (search-only mode)")
	}
}

func TestBuildPreviewPrompt_PinnedSkillsHybrid(t *testing.T) {
	ag := baseAgent()
	ag.OtherConfig = []byte(`{"pinned_skills":["deploy"]}`)
	r := BuildPreviewPrompt(context.Background(), ag, PromptFull, "", PreviewDeps{
		SkillsLoader: &mockSkillsLoader{
			pinned:  "<skill name=\"deploy\">Deploy to prod</skill>",
			summary: "<available_skills>\n<skill name=\"git\">Git ops</skill>\n</available_skills>",
		},
	})
	if !strings.Contains(r.Prompt, "deploy") || !strings.Contains(r.Prompt, "Pinned skills") {
		t.Error("expected pinned skills section in prompt")
	}
}

func TestBuildPreviewPrompt_SkillsFromAccessStore(t *testing.T) {
	ag := baseAgent()
	// Loader summary must NOT be used when an access store is wired — per-tenant
	// skills live in the DB, not the filesystem loader.
	loader := &mockSkillsLoader{
		summary: "<available_skills><skill name=\"loader-must-not-be-used\">x</skill></available_skills>",
	}
	r := BuildPreviewPrompt(context.Background(), ag, PromptFull, "user1", PreviewDeps{
		SkillsLoader: loader,
		SkillAccessStore: &mockSkillAccessStore{
			accessible: []store.SkillInfo{{Slug: "near", Name: "near", Description: "near token price", Status: "active"}},
		},
	})
	// Summary is sourced from the access store (DB), not the loader.
	if !strings.Contains(r.Prompt, "near token price") {
		t.Errorf("expected access-store skill in prompt, got: %s", r.Prompt)
	}
	if loader.capturedAllow != nil {
		t.Errorf("loader must not be consulted for skills when an access store is set, got %v", loader.capturedAllow)
	}
}

func TestBuildPreviewPrompt_SkillAccessStoreError(t *testing.T) {
	ag := baseAgent()
	loader := &mockSkillsLoader{
		summary: "<available_skills><skill name=\"s\">desc</skill></available_skills>",
	}
	r := BuildPreviewPrompt(context.Background(), ag, PromptFull, "user1", PreviewDeps{
		SkillsLoader:     loader,
		SkillAccessStore: &mockSkillAccessStore{err: errors.New("db error")},
	})
	if r.Prompt == "" {
		t.Fatal("expected non-empty prompt on SkillAccessStore error")
	}
	// On access-store error we show no skills and do NOT fall back to the loader
	// (it can't see per-tenant skills anyway).
	if strings.Contains(r.Prompt, "<available_skills>") {
		t.Errorf("expected no skills section on access-store error, got: %s", r.Prompt)
	}
	if loader.capturedAllow != nil {
		t.Errorf("loader must not be consulted when an access store is set, got %v", loader.capturedAllow)
	}
}

func TestBuildPreviewPrompt_MCPToolDescs(t *testing.T) {
	r := BuildPreviewPrompt(context.Background(), baseAgent(), PromptFull, "", PreviewDeps{
		ToolLister: &mockToolLister{
			tools: map[string]string{
				"read_file":    "Read a file",
				"mcp_pg_query": "Run PostgreSQL queries",
			},
		},
	})
	if !strings.Contains(r.Prompt, "mcp_pg_query") {
		t.Error("expected MCP tool description in prompt")
	}
}

func TestBuildPreviewPrompt_MCPToolSearchExcluded(t *testing.T) {
	r := BuildPreviewPrompt(context.Background(), baseAgent(), PromptFull, "", PreviewDeps{
		ToolLister: &mockToolLister{
			tools: map[string]string{
				"read_file":       "Read a file",
				"mcp_tool_search": "Search MCP tools",
			},
		},
	})
	if strings.Contains(r.Prompt, "Search MCP tools") {
		t.Error("mcp_tool_search should not appear in MCP tool descriptions")
	}
}

func TestBuildPreviewPrompt_AliasExclusion(t *testing.T) {
	r := BuildPreviewPrompt(context.Background(), baseAgent(), PromptFull, "", PreviewDeps{
		ToolLister: &mockToolLister{
			tools: map[string]string{
				"read_file": "Read a file",
				"Read":      "Alias for read_file",
				"exec":      "Execute shell",
				"Bash":      "Alias for exec",
			},
			aliases: map[string]string{
				"Read": "read_file",
				"Bash": "exec",
			},
		},
	})
	if strings.Contains(r.Prompt, "- Read\n") || strings.Contains(r.Prompt, "- Bash\n") {
		t.Error("aliases should be excluded from tool list")
	}
}

func TestBuildPreviewPrompt_SkillManageGating(t *testing.T) {
	ag := baseAgent()
	r := BuildPreviewPrompt(context.Background(), ag, PromptFull, "", PreviewDeps{
		ToolLister: &mockToolLister{
			tools: map[string]string{
				"read_file":    "Read a file",
				"skill_manage": "Manage skills",
			},
		},
	})
	if strings.Contains(r.Prompt, "skill_manage") {
		t.Error("skill_manage should be excluded when skill_evolve is off")
	}
}

func TestBuildPreviewPrompt_SkillManageEnabled(t *testing.T) {
	ag := baseAgent()
	ag.SkillEvolve = true
	r := BuildPreviewPrompt(context.Background(), ag, PromptFull, "", PreviewDeps{
		ToolLister: &mockToolLister{
			tools: map[string]string{
				"read_file":    "Read a file",
				"skill_manage": "Manage skills",
				"skill_search": "Search skills",
			},
		},
	})
	if !strings.Contains(r.Prompt, "skill_manage") {
		t.Error("skill_manage should be present when skill_evolve is on")
	}
}

func TestBuildPreviewPrompt_ToolPolicyDeny(t *testing.T) {
	ag := baseAgent()
	ag.ToolsConfig = []byte(`{"deny":["exec","web_fetch"]}`)
	r := BuildPreviewPrompt(context.Background(), ag, PromptFull, "", PreviewDeps{
		ToolLister: &mockToolLister{
			tools: map[string]string{
				"read_file": "Read a file",
				"exec":      "Execute shell",
				"web_fetch": "Fetch web page",
			},
		},
	})
	if strings.Contains(r.Prompt, "- exec\n") {
		t.Error("denied tool 'exec' should be excluded")
	}
	if strings.Contains(r.Prompt, "- web_fetch\n") {
		t.Error("denied tool 'web_fetch' should be excluded")
	}
	if !strings.Contains(r.Prompt, "read_file") {
		t.Error("non-denied tool 'read_file' should be present")
	}
}

func TestBuildPreviewPrompt_ToolDefs(t *testing.T) {
	r := BuildPreviewPrompt(context.Background(), baseAgent(), PromptFull, "", PreviewDeps{
		ToolLister: &mockToolLister{
			tools: map[string]string{
				"read_file": "Read a file",
				"exec":      "Execute shell",
			},
			aliases: map[string]string{
				"Read": "read_file",
			},
		},
	})
	// Should have canonical tools + aliases in tool defs
	if len(r.ToolDefs) != 3 { // read_file + exec + Read alias
		t.Errorf("expected 3 tool defs (2 canonical + 1 alias), got %d", len(r.ToolDefs))
	}
	// Verify alias is included in defs even though excluded from system prompt
	found := false
	for _, td := range r.ToolDefs {
		if td.Function.Name == "Read" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected alias 'Read' in tool defs")
	}
}
