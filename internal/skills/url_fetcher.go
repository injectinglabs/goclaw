package skills

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// Allowed Content-Type values for a direct .tar.gz URL fetch. application/
// octet-stream is included because many static-file hosts (S3, GH user
// content, generic CDNs) return it for binary downloads.
var allowedTarballContentTypes = map[string]bool{
	"application/gzip":         true,
	"application/x-gzip":       true,
	"application/octet-stream": true,
	"application/x-tar":        true, // common when server doesn't detect gzip layer
}

// FetchURLTarball downloads a direct https://*.tar.gz URL to a temp file,
// validating the host allowlist (via IsHostAllowed) and content type, and
// computing the SHA-256 of the downloaded bytes.
//
// Returns (path, sha256-hex, cleanup, error). Caller MUST defer cleanup.
func FetchURLTarball(ctx context.Context, rawURL string) (string, string, func(), error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", "", noopCleanup, fmt.Errorf("url_fetcher: parse url: %w", err)
	}
	if u.Scheme != "https" {
		return "", "", noopCleanup, ErrUnsupportedScheme
	}
	if !IsHostAllowed(u.Hostname()) {
		return "", "", noopCleanup, fmt.Errorf("%w: %s", ErrSourceHostBlocked, u.Hostname())
	}

	ctx, cancel := context.WithTimeout(ctx, SkillFetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", "", noopCleanup, err
	}
	req.Header.Set("Accept", "application/gzip, application/octet-stream;q=0.5")

	client := &http.Client{Timeout: SkillFetchTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", noopCleanup, fmt.Errorf("url_fetcher: download: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", "", noopCleanup, fmt.Errorf("url_fetcher: status %d", resp.StatusCode)
	}

	// Content-Type check (strip parameters like "; charset=utf-8" — though tar
	// servers shouldn't send those, defensive parsing is cheap).
	ct := strings.ToLower(strings.TrimSpace(strings.SplitN(resp.Header.Get("Content-Type"), ";", 2)[0]))
	if ct != "" && !allowedTarballContentTypes[ct] {
		return "", "", noopCleanup, fmt.Errorf("url_fetcher: unexpected content-type %q", ct)
	}

	path, cleanup, sum, err := spillAndHash(resp.Body, "goclaw-skill-url-tarball-*.tar.gz", MaxSkillTarballBytes)
	if err != nil {
		return "", "", noopCleanup, err
	}
	return path, sum, cleanup, nil
}

// spillAndHash copies the body to a temp file while computing SHA-256.
// Same overflow + cleanup semantics as spillToTempFile.
func spillAndHash(r io.Reader, pattern string, maxBytes int64) (string, func(), string, error) {
	tmp, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", noopCleanup, "", err
	}
	path := tmp.Name()
	cleanup := func() { _ = os.Remove(path) }

	h := sha256.New()
	mw := io.MultiWriter(tmp, h)
	n, copyErr := io.Copy(mw, io.LimitReader(r, maxBytes+1))
	if cerr := tmp.Close(); cerr != nil && copyErr == nil {
		copyErr = cerr
	}
	if copyErr != nil {
		cleanup()
		return "", noopCleanup, "", fmt.Errorf("url_fetcher: copy: %w", copyErr)
	}
	if n > maxBytes {
		cleanup()
		return "", noopCleanup, "", errSkillTarballTooLarge
	}
	return path, cleanup, hex.EncodeToString(h.Sum(nil)), nil
}

