package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestDeliverFile_AttachesExistingFile verifies the happy path: an existing
// workspace file is attached as Media (a download link) and the result is
// silent (no duplicate text spam).
func TestDeliverFile_AttachesExistingFile(t *testing.T) {
	ws := t.TempDir()
	xlsx := filepath.Join(ws, "report.xlsx")
	if err := os.WriteFile(xlsx, []byte("PK\x03\x04 fake xlsx bytes"), 0644); err != nil {
		t.Fatal(err)
	}

	tool := NewDeliverFileTool(ws, true)
	res := tool.Execute(context.Background(), map[string]any{"path": "report.xlsx"})

	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	if len(res.Media) != 1 {
		t.Fatalf("Media len = %d, want 1 (the download attachment)", len(res.Media))
	}
	if res.Media[0].Filename != "report.xlsx" {
		t.Errorf("Filename = %q, want report.xlsx", res.Media[0].Filename)
	}
	if !res.Silent {
		t.Errorf("expected Silent result (the attachment is the payload, not text)")
	}
}

// TestDeliverFile_SandboxAbsolutePathRewrite reproduces the staging failure:
// exec wrote the file at the container path /workspace/report.xlsx (bind-mounted
// to the host workspace), and the model handed deliver_file that absolute path.
// It must rewrite /workspace/... to workspace-relative and deliver, not reject.
func TestDeliverFile_SandboxAbsolutePathRewrite(t *testing.T) {
	ws := t.TempDir() // host workspace (does not start with /workspace)
	if err := os.WriteFile(filepath.Join(ws, "report.xlsx"), []byte("PK fake"), 0644); err != nil {
		t.Fatal(err)
	}
	tool := NewDeliverFileTool(ws, true)
	res := tool.Execute(context.Background(), map[string]any{"path": "/workspace/report.xlsx"})
	if res.IsError {
		t.Fatalf("expected delivery after rewriting sandbox path, got error: %s", res.ForLLM)
	}
	if len(res.Media) != 1 || res.Media[0].Filename != "report.xlsx" {
		t.Fatalf("expected report.xlsx attached, got %+v", res.Media)
	}
}

// TestDeliverFile_BasenameFallback verifies the workspace search fallback: a
// path that doesn't resolve exactly still delivers if the basename exists.
func TestDeliverFile_BasenameFallback(t *testing.T) {
	ws := t.TempDir()
	sub := filepath.Join(ws, "out")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "data.xlsx"), []byte("PK"), 0644); err != nil {
		t.Fatal(err)
	}
	tool := NewDeliverFileTool(ws, true)
	// Wrong dir, right name → fallback finds out/data.xlsx.
	res := tool.Execute(context.Background(), map[string]any{"path": "data.xlsx"})
	if res.IsError {
		t.Fatalf("expected basename-fallback delivery, got error: %s", res.ForLLM)
	}
	if len(res.Media) != 1 {
		t.Fatalf("expected 1 attachment, got %d", len(res.Media))
	}
}

// TestDeliverFile_MissingFile verifies a clear error (not a silent attach) when
// the path doesn't exist — guides the model to create it first.
func TestDeliverFile_MissingFile(t *testing.T) {
	ws := t.TempDir()
	tool := NewDeliverFileTool(ws, true)
	res := tool.Execute(context.Background(), map[string]any{"path": "nope.xlsx"})
	if !res.IsError {
		t.Fatalf("expected error for missing file, got: %+v", res)
	}
	if len(res.Media) != 0 {
		t.Errorf("missing file must not attach media")
	}
}

// TestDeliverFile_RejectsDirectory verifies a directory can't be delivered.
func TestDeliverFile_RejectsDirectory(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "out"), 0755); err != nil {
		t.Fatal(err)
	}
	tool := NewDeliverFileTool(ws, true)
	res := tool.Execute(context.Background(), map[string]any{"path": "out"})
	if !res.IsError {
		t.Fatalf("expected error for directory, got: %+v", res)
	}
}

// TestDeliverFile_RequiresPath verifies the path arg is mandatory.
func TestDeliverFile_RequiresPath(t *testing.T) {
	tool := NewDeliverFileTool(t.TempDir(), true)
	res := tool.Execute(context.Background(), map[string]any{})
	if !res.IsError {
		t.Fatalf("expected error for missing path")
	}
}
