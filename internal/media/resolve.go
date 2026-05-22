package media

import (
	"context"
	"errors"
	"net/url"
	"os"
	"regexp"
	"strings"
)

// staleFileToken matches `?ft=...` or `&ft=...` segments that the HTTP layer
// adds to signed media URLs. Mirrors internal/http.staleTokenRe so we can
// strip the suffix without depending on the http package (which would
// create an import cycle).
var staleFileToken = regexp.MustCompile(`[?&]ft=[^\s)"'<>&]*`)

// mediaIDInPathRe matches the trailing UUID-like media ID in a path or URL
// produced by the media stores. The S3 backend writes
// `<cacheRoot>/<sessionHash>/<id>.<ext>` and signed URLs wrap that path
// in `/v1/files/...`. The capture is the id (UUID v4-shaped) right before
// the extension. The non-greedy ext class avoids swallowing anything
// after the first dot (so `kimi-k2.5` style aliases aren't false
// positives in URLs that happen to share the suffix shape).
var mediaIDInPathRe = regexp.MustCompile(`([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\.[A-Za-z0-9]+(?:$|\?)`)

// ResolveLocalPath turns a user-supplied media reference into a local
// filesystem path the caller can `os.Open`. Handles three real shapes
// seen in prod (`internal/agent/media.go:loadImages` was hitting all
// three on `read_image` / vision flows):
//
//  1. Already a clean local path (FSBackend or S3Backend cache hit on
//     the same instance that uploaded the file). Returns the path as-is
//     once existence is confirmed.
//
//  2. A signed HTTP URL — `/v1/files/<path>?ft=<token>`. The chat layer
//     adds this prefix + query when serving messages to the client; on
//     the next chat.send the client passes the signed form back as the
//     "path" of an attached image. Strip the prefix + query and recurse
//     into local-path resolution.
//
//  3. A clean local path the current instance hasn't seen yet — file is
//     in S3 but `.media-cache/<hash>/<id>.<ext>` is empty because the
//     upload landed on a sibling EC2 in the prod ASG. Extract the
//     media id from the path and ask the Store to download it (which
//     populates the cache and returns the cache path).
//
// Returns ErrNotFound when the path looks like a media ref but the id
// isn't recognized by the store (or the store is FS-only and the file
// really is gone). Returns the input unchanged for paths that don't
// resemble a media-store path at all (e.g. operator-supplied absolute
// paths into the workspace) so the existing access-control logic in
// the calling tool stays in charge.
func ResolveLocalPath(rawPath string, store *Store) (string, error) {
	if rawPath == "" {
		return "", errors.New("media: empty path")
	}

	// Shape 2: signed URL → strip `/v1/files/` prefix and `?ft=` token.
	// SignMediaPath stacks multiple `/v1/files/` prefixes on re-sign,
	// so trim them in a loop (matches the defensive logic on the sign
	// side in internal/http/file_token.go).
	cleaned := rawPath
	if strings.HasPrefix(cleaned, "/v1/files/") {
		for strings.HasPrefix(cleaned, "/v1/files/") {
			cleaned = strings.TrimPrefix(cleaned, "/v1/files")
		}
		cleaned = staleFileToken.ReplaceAllString(cleaned, "")
		cleaned = strings.TrimRight(cleaned, "?&")
	} else if strings.Contains(cleaned, "?ft=") || strings.Contains(cleaned, "&ft=") {
		// Path with a token but no prefix — heal anyway.
		cleaned = staleFileToken.ReplaceAllString(cleaned, "")
		cleaned = strings.TrimRight(cleaned, "?&")
	}

	// Drop any URL escaping that snuck in (e.g. spaces → %20 in
	// document filenames the user uploaded). `os.Open` doesn't unescape.
	if unescaped, err := url.PathUnescape(cleaned); err == nil {
		cleaned = unescaped
	}

	// Shape 1: file exists locally — fast path.
	if _, err := os.Stat(cleaned); err == nil {
		return cleaned, nil
	}

	// Shape 3: file missing, but the path looks like a media-store
	// path. Pull the media id and ask the store to (re)hydrate the
	// cache. Skipped when no store is wired (some test paths and the
	// pre-MediaStore agent flow).
	if store == nil {
		return cleaned, nil
	}
	match := mediaIDInPathRe.FindStringSubmatch(cleaned)
	if len(match) < 2 {
		// Path doesn't look like a media-store path — return as-is and
		// let the caller surface the original "no such file" error.
		// Operator-supplied workspace paths (read_file with an explicit
		// path arg, etc.) hit this branch.
		return cleaned, nil
	}
	mediaID := match[1]
	resolved, err := store.LoadPath(mediaID)
	if err != nil {
		// Fall through to the cleaned path; the caller's
		// os.Open will produce the canonical "no such file or
		// directory" error rather than a media-store wrapped one.
		return cleaned, err
	}
	return resolved, nil
}

// ResolveLocalPathCtx is the context-aware companion to ResolveLocalPath.
// The Store façade currently swallows the context (it uses
// context.Background internally) — this signature exists so callers
// inside an LLM iteration loop have a deadline-aware seam to plug into
// once the façade gains real context support.
func ResolveLocalPathCtx(_ context.Context, rawPath string, store *Store) (string, error) {
	return ResolveLocalPath(rawPath, store)
}
