package http

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/nextlevelbuilder/goclaw/internal/channels/media"
	"github.com/nextlevelbuilder/goclaw/internal/i18n"
	mediastore "github.com/nextlevelbuilder/goclaw/internal/media"
)

// MediaImportHandler exposes POST /v1/internal/media/import — an
// instance-internal endpoint for trusted backend services (document-mcp,
// future workers) to push generated artefacts into the goclaw MediaStore
// without holding S3 credentials of their own.
//
// Why this exists: services like document-mcp historically wrote to their
// own S3 prefix (`tenants/<slug>/<user>/created/...`) and returned a
// pre-signed S3 URL embedded in the chat message. Two problems with that:
//
//  1. The pre-signed URL has a fixed TTL — when the user re-opens the chat
//     a week later, the link is dead even though the file might still be
//     in S3.
//  2. Inconsistent UX/code paths: attached files came back through
//     `/v1/files/...?ft=<token>` (re-signed on every history fetch),
//     created files came back as direct S3 URLs.
//
// With this endpoint, every internal artefact lands in the same MediaStore
// (S3 bucket + cache mirror) the chat-attached files use. The caller gets
// a stable media id + local cache path. Storing the clean path in chat
// history lets goclaw's SignMediaPath produce a fresh signed URL every
// time the user views the message — same recipe attached files already
// use.
//
// Auth: the gateway bearer token (`GOCLAW_GATEWAY_TOKEN`). The same token
// auth-proxy + connectors-mcp + future internal services already use.
// Public callers must never touch this endpoint.
type MediaImportHandler struct {
	store *mediastore.Store
}

func NewMediaImportHandler(store *mediastore.Store) *MediaImportHandler {
	return &MediaImportHandler{store: store}
}

func (h *MediaImportHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/internal/media/import", h.auth(h.handleImport))
}

func (h *MediaImportHandler) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Internal endpoint: require the gateway bearer token. We don't
		// fall through to API-key / pairing / no-auth modes here — the
		// route is opt-in for trusted callers only.
		if pkgGatewayToken == "" {
			http.Error(w, "internal endpoint disabled (no gateway token configured)", http.StatusServiceUnavailable)
			return
		}
		if !tokenMatch(extractBearerToken(r), pkgGatewayToken) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (h *MediaImportHandler) handleImport(w http.ResponseWriter, r *http.Request) {
	locale := extractLocale(r)
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)

	if h.store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "media store not configured"})
		return
	}

	if err := r.ParseMultipartForm(maxUploadSize); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgFileTooLarge)})
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgMissingFileField)})
		return
	}
	defer file.Close()

	origName := filepath.Base(header.Filename)
	if origName == "." || origName == "/" || strings.Contains(origName, "..") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(locale, i18n.MsgInvalidFilename)})
		return
	}

	// session_key is required so the file lands under the right
	// {sessionHash}/ in S3 and is reachable via the same SignMediaPath
	// flow that chat-attached files use. Callers (document-mcp, etc.)
	// should pass the goclaw chat session_key they got via X-Actor-*
	// headers — or a synthetic `created:<tenant>:<user>` key when the
	// artefact is not bound to a specific chat turn.
	sessionKey := strings.TrimSpace(r.FormValue("session_key"))
	if sessionKey == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "session_key is required"})
		return
	}

	mimeType := strings.TrimSpace(r.FormValue("content_type"))
	if mimeType == "" {
		mimeType = media.DetectMIMEType(origName)
	}

	hintExt := filepath.Ext(origName)
	if hintExt == "" {
		hintExt = ".bin"
	}

	id, dst, err := h.store.SaveReader(sessionKey, mimeType, file, hintExt)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": fmt.Sprintf("persist failed: %v", err)})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"media_id":  id,
		"path":      dst,
		"mime_type": mimeType,
		"filename":  origName,
	})
}
