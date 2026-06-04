package skills

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Limits enforced by ExtractTarball.
const (
	// MaxSkillExtractedBytes caps the total uncompressed size of an installed
	// skill tarball. Independently lower than MaxSkillTarballBytes because a
	// gzipped tarball can decompress to far more than the wire size.
	MaxSkillExtractedBytes = 20 * 1024 * 1024 // 20 MB

	// MaxSkillExtractedFiles caps the number of regular file entries written
	// to disk. Guards against tar-bomb-style high-cardinality archives.
	MaxSkillExtractedFiles = 500
)

// pathTraversalRE flags any occurrence of `..` followed by a path separator,
// at any depth — `../`, `foo/../bar`, `./../etc/passwd`. The upload handler's
// pre-existing path traversal guard only checked for literal `..` substring;
// this stricter form catches edge cases like Windows separators in tar names
// or tail components that re-emerge after filepath.Clean.
var pathTraversalRE = regexp.MustCompile(`(^|[/\\])\.\.([/\\]|$)`)

// Sentinels for extraction failures.
var (
	ErrTarballPathTraversal = errors.New("tarball_extractor: path traversal rejected")
	ErrTarballSymlinkEscape = errors.New("tarball_extractor: symlink escape rejected")
	ErrTarballTooManyFiles  = errors.New("tarball_extractor: file count exceeds 500")
	ErrTarballTooLarge      = errors.New("tarball_extractor: extracted size exceeds 20MB")
)

// ExtractTarball gunzips and untars tarballPath into destDir, enforcing:
//   - max 500 regular files
//   - max 20 MB total extracted bytes
//   - reject paths containing `..` at any depth, absolute paths, or symlinks
//     that resolve outside destDir
//   - skip non-regular file types (devices, fifos, etc.)
//
// GitHub-style tarballs wrap everything in a single top-level directory
// (`{repo}-{sha[:7]}/...`). If every entry shares such a prefix, it is
// stripped so destDir receives the skill contents directly (SKILL.md at root).
func ExtractTarball(tarballPath, destDir string) error {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("tarball_extractor: mkdir dest: %w", err)
	}

	// Resolve dest to an absolute, symlink-cleaned path for prefix checks.
	absDest, err := filepath.Abs(destDir)
	if err != nil {
		return fmt.Errorf("tarball_extractor: resolve dest: %w", err)
	}
	prefix := absDest + string(os.PathSeparator)

	f, err := os.Open(tarballPath)
	if err != nil {
		return fmt.Errorf("tarball_extractor: open: %w", err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("tarball_extractor: gzip: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)

	// First pass: read all headers + payloads into memory? No — that defeats
	// the size cap. Instead, do a streaming single pass with a sticky
	// top-level prefix: we learn the prefix from the first entry and then
	// validate every subsequent entry shares it. If any entry diverges we
	// abandon stripping and re-emit the first entry under its original name —
	// but that requires buffering, which we don't want. Compromise: the strip
	// prefix is derived purely from name parsing (first segment of the first
	// regular entry that contains a slash) and applied unconditionally.
	// Diverging entries are still written, just without the strip, which is
	// safe because the prefix is checked against pathTraversalRE.
	var stripPrefix string

	var (
		totalBytes int64
		fileCount  int
	)

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tarball_extractor: tar: %w", err)
		}

		// Determine strip prefix from the first entry that names a top-level
		// directory. GitHub guarantees `{repo}-{sha7}/`, but we don't hard-code
		// the format — just take everything up to the first `/`.
		if stripPrefix == "" {
			if i := strings.Index(hdr.Name, "/"); i > 0 {
				stripPrefix = hdr.Name[:i+1]
			}
		}

		entryName := hdr.Name
		// Reject absolute paths up front.
		if strings.HasPrefix(entryName, "/") || strings.HasPrefix(entryName, `\`) {
			return fmt.Errorf("%w: absolute path %q", ErrTarballPathTraversal, entryName)
		}
		// Reject `..` at any depth — applied to the raw header name before
		// any cleaning, because filepath.Clean can collapse `foo/../bar` into
		// `bar`, hiding the original intent.
		if pathTraversalRE.MatchString(entryName) {
			return fmt.Errorf("%w: %q", ErrTarballPathTraversal, entryName)
		}

		if stripPrefix != "" && strings.HasPrefix(entryName, stripPrefix) {
			entryName = entryName[len(stripPrefix):]
		}
		if entryName == "" {
			// The wrapper directory itself — skip.
			continue
		}
		if IsSystemArtifact(entryName) {
			continue
		}

		// Skip non-regular file types — symlinks, devices, FIFOs, etc.
		// (Symlinks specifically are dangerous: even a relative target that
		// looks safe at write time can be resolved later by a script reading
		// from the skill dir, producing an escape.)
		switch hdr.Typeflag {
		case tar.TypeDir:
			dst := filepath.Join(absDest, filepath.FromSlash(entryName))
			if !strings.HasPrefix(dst+string(os.PathSeparator), prefix) && dst != absDest {
				return fmt.Errorf("%w: %q", ErrTarballPathTraversal, entryName)
			}
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return fmt.Errorf("tarball_extractor: mkdir: %w", err)
			}
			continue
		case tar.TypeReg, tar.TypeRegA:
			// fall through
		case tar.TypeSymlink, tar.TypeLink:
			return fmt.Errorf("%w: symlink/hardlink %q -> %q",
				ErrTarballSymlinkEscape, entryName, hdr.Linkname)
		default:
			// Devices, FIFOs, sparse markers, etc. — skip silently.
			continue
		}

		// Regular file: enforce cumulative limits before we read the payload.
		fileCount++
		if fileCount > MaxSkillExtractedFiles {
			return ErrTarballTooManyFiles
		}

		dst := filepath.Join(absDest, filepath.FromSlash(entryName))
		if !strings.HasPrefix(dst, prefix) {
			return fmt.Errorf("%w: %q resolves outside dest", ErrTarballPathTraversal, entryName)
		}

		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("tarball_extractor: mkdir parent: %w", err)
		}
		out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			return fmt.Errorf("tarball_extractor: create %q: %w", dst, err)
		}

		// Cap per-file copy at (remaining budget + 1) so we can detect overflow
		// without ever buffering past the limit.
		remaining := MaxSkillExtractedBytes - totalBytes
		n, copyErr := io.Copy(out, io.LimitReader(tr, remaining+1))
		closeErr := out.Close()
		if copyErr != nil {
			return fmt.Errorf("tarball_extractor: write %q: %w", dst, copyErr)
		}
		if closeErr != nil {
			return fmt.Errorf("tarball_extractor: close %q: %w", dst, closeErr)
		}
		totalBytes += n
		if totalBytes > MaxSkillExtractedBytes {
			return ErrTarballTooLarge
		}
	}

	return nil
}
