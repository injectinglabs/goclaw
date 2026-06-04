package tools

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

// SandboxCwd maps the current effective workspace (from context) to its
// corresponding path inside the sandbox container. The sandbox mounts the
// global workspace root at containerBase (usually "/workspace"). This function
// computes the relative path from globalWorkspace to the context workspace
// and joins it with containerBase.
//
// Example: globalWorkspace="/app/workspace", ctx workspace="/app/workspace/agent-a/user-123"
// → returns "/workspace/agent-a/user-123"
func SandboxCwd(ctx context.Context, globalWorkspace, containerBase string) (string, error) {
	ws := ToolWorkspaceFromCtx(ctx)
	if ws == "" {
		// No per-request workspace — fall back to container root.
		return containerBase, nil
	}

	rel, err := filepath.Rel(globalWorkspace, ws)
	if err != nil || strings.HasPrefix(filepath.Clean(rel), "..") {
		return "", fmt.Errorf("workspace %q is outside global mount %q", ws, globalWorkspace)
	}

	if rel == "." {
		return containerBase, nil
	}
	return filepath.Join(containerBase, rel), nil
}

// isAllowListedHostPath reports whether p is an absolute path under one of the
// tool's extra allow-listed host prefixes (skills-store, dataDir/tenants,
// cli-workspaces, user-configured paths). Those live on the goclaw data volume,
// which is NOT mounted into the sandbox container — only the workspace is. When
// exec is sandboxed, reads of these paths (a skill's SKILL.md or bundled
// assets) must be served host-side, since the sandbox container cannot see
// them; without this the use_skill → read_file SKILL.md flow fails in-sandbox.
//
// This only chooses host vs. sandbox resolution — the host read path still
// validates against the same allow-prefixes, so it does not widen file access.
// Relative paths return false: they resolve against the workspace, which IS
// mounted in the sandbox and should be read there.
func isAllowListedHostPath(p string, prefixes []string) bool {
	if !filepath.IsAbs(p) {
		return false
	}
	clean := filepath.Clean(p)
	for _, prefix := range prefixes {
		if prefix == "" {
			continue
		}
		pc := filepath.Clean(prefix)
		if clean == pc || strings.HasPrefix(clean, pc+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// ResolveSandboxPath resolves a tool-provided path (relative or absolute)
// against the sandbox container CWD. If the path is relative, it is joined
// with containerCwd. Absolute paths are returned as-is (the sandbox
// filesystem already restricts access to the mounted volume).
func ResolveSandboxPath(path, containerCwd string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(containerCwd, path)
}
