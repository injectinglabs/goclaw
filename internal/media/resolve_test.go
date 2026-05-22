package media

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveLocalPath_StripsSignedURLWrapper(t *testing.T) {
	dir := t.TempDir()
	realPath := filepath.Join(dir, "sub", "image.jpg")
	if err := os.MkdirAll(filepath.Dir(realPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(realPath, []byte("jpeg-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	signed := "/v1/files" + realPath + "?ft=abc123.1779411693"
	got, err := ResolveLocalPath(signed, nil)
	if err != nil {
		t.Fatalf("expected resolution to succeed, got: %v", err)
	}
	if got != realPath {
		t.Fatalf("expected %q, got %q", realPath, got)
	}
}

func TestResolveLocalPath_StripsBareFtToken(t *testing.T) {
	dir := t.TempDir()
	realPath := filepath.Join(dir, "img.png")
	if err := os.WriteFile(realPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := ResolveLocalPath(realPath+"?ft=token.123", nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != realPath {
		t.Fatalf("expected %q got %q", realPath, got)
	}
}

func TestResolveLocalPath_StackedV1FilesPrefixes(t *testing.T) {
	dir := t.TempDir()
	realPath := filepath.Join(dir, "x.gif")
	if err := os.WriteFile(realPath, []byte("g"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Legacy bug: signed URL with the prefix doubled up.
	stacked := "/v1/files/v1/files" + realPath
	got, err := ResolveLocalPath(stacked, nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != realPath {
		t.Fatalf("expected %q got %q", realPath, got)
	}
}

func TestResolveLocalPath_CleanPathPassthrough(t *testing.T) {
	dir := t.TempDir()
	realPath := filepath.Join(dir, "doc.pdf")
	if err := os.WriteFile(realPath, []byte("p"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := ResolveLocalPath(realPath, nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != realPath {
		t.Fatalf("expected %q got %q", realPath, got)
	}
}

func TestResolveLocalPath_URLEncodedSpaces(t *testing.T) {
	dir := t.TempDir()
	realPath := filepath.Join(dir, "with space.jpg")
	if err := os.WriteFile(realPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	encoded := "/v1/files" + filepath.ToSlash(filepath.Dir(realPath)) + "/with%20space.jpg?ft=t"
	got, err := ResolveLocalPath(encoded, nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	// On Windows filepath.Join uses `\`; the resolved path is whatever
	// os.Stat accepts. Just verify it ends with the literal filename.
	if filepath.Base(got) != "with space.jpg" {
		t.Fatalf("expected resolved basename 'with space.jpg', got %q", got)
	}
}

func TestResolveLocalPath_EmptyInputErrors(t *testing.T) {
	if _, err := ResolveLocalPath("", nil); err == nil {
		t.Fatal("expected error on empty input, got nil")
	}
}

func TestResolveLocalPath_NoMediaIDPassesPathThrough(t *testing.T) {
	// Path doesn't exist and doesn't look like a media-store path.
	// Resolver should return the cleaned path so the caller's os.Open
	// produces the canonical "no such file" error.
	got, err := ResolveLocalPath("/v1/files/app/workspace/not-a-uuid.jpg?ft=x", nil)
	if err != nil {
		t.Fatalf("expected no error (caller will see ENOENT later), got %v", err)
	}
	if got != "/app/workspace/not-a-uuid.jpg" {
		t.Fatalf("expected '/app/workspace/not-a-uuid.jpg', got %q", got)
	}
}
