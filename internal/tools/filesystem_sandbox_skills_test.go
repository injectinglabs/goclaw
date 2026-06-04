package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsAllowListedHostPath(t *testing.T) {
	prefixes := []string{"/app/data/tenants", "/app/data/skills-store", "/home/u/.agents/skills"}
	cases := []struct {
		path string
		want bool
	}{
		{"/app/data/tenants/t1/skills-store/my-skill/1/SKILL.md", true}, // skills-store under tenants
		{"/app/data/skills-store/global/1/SKILL.md", true},              // master skills-store
		{"/app/data/tenants", true},                                     // exact prefix
		{"/app/data/tenantsX/evil", false},                              // sibling, not a path segment boundary
		{"/app/workspace/agent/file.txt", false},                       // workspace path → sandbox
		{"SKILL.md", false},                                             // relative → workspace → sandbox
		{"scripts/run.py", false},                                       // relative → sandbox
		{"/etc/passwd", false},                                          // absolute but not allow-listed
	}
	for _, c := range cases {
		if got := isAllowListedHostPath(c.path, prefixes); got != c.want {
			t.Errorf("isAllowListedHostPath(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

// TestReadFile_SandboxServesSkillFilesHostSide proves that when exec is
// sandboxed, an absolute skills-store path is read on the host (not routed into
// the sandbox container, which doesn't mount it). The fake manager would be
// used only if the read wrongly went to the sandbox; instead we get the real
// file content back.
func TestReadFile_SandboxServesSkillFilesHostSide(t *testing.T) {
	skillsDir := t.TempDir()
	skillFile := filepath.Join(skillsDir, "my-skill", "1", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skillFile), 0o755); err != nil {
		t.Fatal(err)
	}
	const body = "# My Skill\nrun: python3 do_thing.py"
	if err := os.WriteFile(skillFile, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := NewSandboxedReadFileTool(t.TempDir(), true, &fakeManager{sb: &fakeSandbox{out: "FROM_SANDBOX"}})
	tool.AllowPaths(skillsDir) // mirrors gateway wiring of skillsAllowPaths

	ctx := WithToolSandboxKey(context.Background(), "agent:main:web:direct:1")
	res := tool.Execute(ctx, map[string]any{"path": skillFile})

	if !strings.Contains(res.ForLLM, "run: python3 do_thing.py") {
		t.Errorf("expected host-side SKILL.md content, got: %s", res.ForLLM)
	}
	if strings.Contains(res.ForLLM, "FROM_SANDBOX") {
		t.Errorf("skill file was wrongly routed through the sandbox: %s", res.ForLLM)
	}
}
