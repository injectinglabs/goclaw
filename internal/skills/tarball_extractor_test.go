package skills

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildTar returns a gzipped tar containing the given entries.
type tarEntry struct {
	Name     string
	Typeflag byte
	Mode     int64
	Body     []byte
	Linkname string
}

func buildTar(t *testing.T, entries []tarEntry) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{
			Name:     e.Name,
			Mode:     e.Mode,
			Typeflag: e.Typeflag,
			Size:     int64(len(e.Body)),
			Linkname: e.Linkname,
		}
		if hdr.Mode == 0 {
			hdr.Mode = 0o644
		}
		if hdr.Typeflag == 0 {
			hdr.Typeflag = tar.TypeReg
		}
		if hdr.Typeflag == tar.TypeDir {
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header %q: %v", e.Name, err)
		}
		if hdr.Typeflag == tar.TypeReg && len(e.Body) > 0 {
			if _, err := tw.Write(e.Body); err != nil {
				t.Fatalf("write body %q: %v", e.Name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gz close: %v", err)
	}
	path := filepath.Join(t.TempDir(), "skill.tar.gz")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write tarball: %v", err)
	}
	return path
}

func TestTarballExtractor_NormalExtraction(t *testing.T) {
	path := buildTar(t, []tarEntry{
		{Name: "myskill-abc1234/", Typeflag: tar.TypeDir},
		{Name: "myskill-abc1234/SKILL.md", Body: []byte("---\nname: x\n---\nbody\n")},
		{Name: "myskill-abc1234/scripts/", Typeflag: tar.TypeDir},
		{Name: "myskill-abc1234/scripts/run.py", Body: []byte("print('hi')\n")},
	})
	dst := t.TempDir()
	if err := ExtractTarball(path, dst); err != nil {
		t.Fatalf("ExtractTarball error: %v", err)
	}
	for _, want := range []string{"SKILL.md", "scripts/run.py"} {
		if _, err := os.Stat(filepath.Join(dst, want)); err != nil {
			t.Errorf("missing %q after extract: %v", want, err)
		}
	}
	// Wrapper dir should NOT exist under dst — strip prefix logic.
	if _, err := os.Stat(filepath.Join(dst, "myskill-abc1234")); err == nil {
		t.Errorf("wrapper dir leaked into destination")
	}
}

func TestTarballExtractor_PathTraversalRejected(t *testing.T) {
	path := buildTar(t, []tarEntry{
		{Name: "skill/SKILL.md", Body: []byte("ok")},
		{Name: "skill/../../etc/passwd", Body: []byte("evil")},
	})
	dst := t.TempDir()
	err := ExtractTarball(path, dst)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrTarballPathTraversal) {
		t.Fatalf("err = %v, want ErrTarballPathTraversal", err)
	}
}

func TestTarballExtractor_AbsolutePathRejected(t *testing.T) {
	path := buildTar(t, []tarEntry{
		{Name: "/etc/passwd", Body: []byte("evil")},
	})
	dst := t.TempDir()
	err := ExtractTarball(path, dst)
	if err == nil || !errors.Is(err, ErrTarballPathTraversal) {
		t.Fatalf("err = %v, want ErrTarballPathTraversal", err)
	}
}

func TestTarballExtractor_SymlinkRejected(t *testing.T) {
	path := buildTar(t, []tarEntry{
		{Name: "skill/", Typeflag: tar.TypeDir},
		{Name: "skill/SKILL.md", Body: []byte("ok")},
		{Name: "skill/escape", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"},
	})
	dst := t.TempDir()
	err := ExtractTarball(path, dst)
	if err == nil || !errors.Is(err, ErrTarballSymlinkEscape) {
		t.Fatalf("err = %v, want ErrTarballSymlinkEscape", err)
	}
}

func TestTarballExtractor_HardlinkRejected(t *testing.T) {
	path := buildTar(t, []tarEntry{
		{Name: "skill/SKILL.md", Body: []byte("ok")},
		{Name: "skill/hard", Typeflag: tar.TypeLink, Linkname: "../../etc/passwd"},
	})
	dst := t.TempDir()
	err := ExtractTarball(path, dst)
	if err == nil || !errors.Is(err, ErrTarballSymlinkEscape) {
		t.Fatalf("err = %v, want ErrTarballSymlinkEscape", err)
	}
}

func TestTarballExtractor_TooManyFiles(t *testing.T) {
	// Generate MaxSkillExtractedFiles + 5 small entries.
	entries := make([]tarEntry, 0, MaxSkillExtractedFiles+10)
	entries = append(entries, tarEntry{Name: "skill/", Typeflag: tar.TypeDir})
	for i := 0; i < MaxSkillExtractedFiles+5; i++ {
		entries = append(entries, tarEntry{
			Name: fmt.Sprintf("skill/f%04d.txt", i),
			Body: []byte("x"),
		})
	}
	path := buildTar(t, entries)
	dst := t.TempDir()
	err := ExtractTarball(path, dst)
	if err == nil || !errors.Is(err, ErrTarballTooManyFiles) {
		t.Fatalf("err = %v, want ErrTarballTooManyFiles", err)
	}
}

func TestTarballExtractor_TooLarge(t *testing.T) {
	// One file just over the byte limit.
	big := bytes.Repeat([]byte("A"), MaxSkillExtractedBytes+1024)
	path := buildTar(t, []tarEntry{
		{Name: "skill/SKILL.md", Body: []byte("---\nname: x\n---\n")},
		{Name: "skill/big.bin", Body: big},
	})
	dst := t.TempDir()
	err := ExtractTarball(path, dst)
	if err == nil || !errors.Is(err, ErrTarballTooLarge) {
		t.Fatalf("err = %v, want ErrTarballTooLarge", err)
	}
}

func TestTarballExtractor_RejectsSubpathTraversal(t *testing.T) {
	// Hidden ../ deep inside the path.
	path := buildTar(t, []tarEntry{
		{Name: "skill/sub/../../../escape.txt", Body: []byte("x")},
	})
	dst := t.TempDir()
	err := ExtractTarball(path, dst)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrTarballPathTraversal) {
		t.Fatalf("err = %v, want ErrTarballPathTraversal", err)
	}
}

// Ensure system artifacts are silently skipped.
func TestTarballExtractor_SkipsSystemArtifacts(t *testing.T) {
	path := buildTar(t, []tarEntry{
		{Name: "skill/SKILL.md", Body: []byte("ok")},
		{Name: "skill/__MACOSX/._SKILL.md", Body: []byte("junk")},
		{Name: "skill/.DS_Store", Body: []byte("junk")},
	})
	dst := t.TempDir()
	if err := ExtractTarball(path, dst); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, ".DS_Store")); err == nil {
		t.Errorf(".DS_Store should be skipped")
	}
	// __MACOSX/* paths should never be written either.
	entries, _ := os.ReadDir(dst)
	for _, e := range entries {
		if strings.Contains(e.Name(), "MACOSX") {
			t.Errorf("__MACOSX leaked into destination: %s", e.Name())
		}
	}
}
