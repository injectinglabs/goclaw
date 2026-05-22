package http

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/channels/media"
	"github.com/nextlevelbuilder/goclaw/internal/i18n"
	mediastore "github.com/nextlevelbuilder/goclaw/internal/media"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

const (
	// maxUploadSize is the default max upload size (50MB).
	maxUploadSize int64 = 50 * 1024 * 1024
)

// MediaUploadHandler handles media file uploads for WebSocket clients.
//
// Files land directly in the configured MediaStore (S3-backed in prod, FS
// in dev/test). The returned path is the local cache mirror — clean,
// UUID-shaped, and recoverable on any sibling instance via
// mediastore.ResolveLocalPath shape 3. Without the store wired we fall
// back to a /tmp scratch file, which works on a single-instance
// deployment but breaks under an ASG (the chat.send WS message may land
// on a sibling that has no view of this instance's /tmp).
type MediaUploadHandler struct {
	store *mediastore.Store
}

// NewMediaUploadHandler creates a media upload handler. Pass a nil store
// only in tests / single-instance dev setups; production wiring must
// supply one so cross-instance reads can hydrate from S3.
func NewMediaUploadHandler(store *mediastore.Store) *MediaUploadHandler {
	return &MediaUploadHandler{store: store}
}

// RegisterRoutes registers the upload endpoint.
func (h *MediaUploadHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/media/upload", h.auth(h.handleUpload))
}

func (h *MediaUploadHandler) auth(next http.HandlerFunc) http.HandlerFunc {
	return requireAuth("", next)
}

func (h *MediaUploadHandler) handleUpload(w http.ResponseWriter, r *http.Request) {
	locale := extractLocale(r)
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)

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

	ext := filepath.Ext(origName)
	if ext == "" {
		ext = ".bin"
	}

	tmpName := fmt.Sprintf("ws_upload_%d%s", time.Now().UnixNano(), ext)
	tmpPath := filepath.Join(os.TempDir(), tmpName)

	out, err := os.Create(tmpPath)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": i18n.T(locale, i18n.MsgInternalError, "failed to create temp file")})
		return
	}

	if _, err := io.Copy(out, file); err != nil {
		out.Close()
		os.Remove(tmpPath)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": i18n.T(locale, i18n.MsgInternalError, "failed to save file")})
		return
	}
	out.Close()

	mimeType := media.DetectMIMEType(origName)

	// Promote /tmp scratch into MediaStore so the file is recoverable on
	// any instance (S3 backend on prod). Without a store this is a no-op
	// and we fall back to the /tmp path — fine for single-instance dev
	// but unsafe under an ASG.
	finalPath := tmpPath
	if h.store != nil {
		sessionKey := uploadSessionKey(r)
		_, dst, err := h.store.SaveFile(sessionKey, tmpPath, mimeType)
		if err == nil {
			finalPath = dst
		}
		// best-effort cleanup of the scratch file; ignore errors.
		_ = os.Remove(tmpPath)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"path":      finalPath,
		"mime_type": mimeType,
		"filename":  origName,
	})
}

// uploadSessionKey scopes pre-chat uploads to (tenant, user) so the
// file is co-tenant on cleanup but globally addressable by its media id
// from any instance in the ASG.
func uploadSessionKey(r *http.Request) string {
	ctx := r.Context()
	tid := store.TenantIDFromContext(ctx).String()
	uid := store.UserIDFromContext(ctx)
	if uid == "" {
		uid = "anon"
	}
	return fmt.Sprintf("upload:%s:%s", tid, uid)
}
