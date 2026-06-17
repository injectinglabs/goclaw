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
