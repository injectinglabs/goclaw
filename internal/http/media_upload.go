package http

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/nextlevelbuilder/goclaw/internal/channels/media"
	"github.com/nextlevelbuilder/goclaw/internal/i18n"
	mediastore "github.com/nextlevelbuilder/goclaw/internal/media"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

const (
	// maxUploadSize is the default max upload size (50MB).
	maxUploadSize int64 = 50 * 1024 * 1024
)

// MediaUploadHandler accepts file uploads from any client (SPA, Chrome
// extension, etc.) for any media kind (image, document, audio, video).
//
// The multipart body is streamed straight into the configured Store —
// S3-backed in prod, FS in dev. No /tmp scratch file. The returned path
// is the local cache mirror, UUID-shaped, recognized by
// mediastore.ResolveLocalPath shape 3, so the subsequent chat.send WS
// message can land on any sibling instance in the prod ASG and still
// resolve the file (manifest in S3 lets LocalPath hydrate the local
// cache on demand).
//
// On dev/test setups without a store wired the handler hard-fails
// rather than silently falling back to /tmp — that historical fallback
// was the root cause of the prod bug we're fixing here, and a noisy
// 500 in dev beats a heisenbug in prod.
type MediaUploadHandler struct {
	store *mediastore.Store
}

func NewMediaUploadHandler(store *mediastore.Store) *MediaUploadHandler {
	return &MediaUploadHandler{store: store}
}

func (h *MediaUploadHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/media/upload", h.auth(h.handleUpload))
}

func (h *MediaUploadHandler) auth(next http.HandlerFunc) http.HandlerFunc {
	return requireAuth("", next)
}

func (h *MediaUploadHandler) handleUpload(w http.ResponseWriter, r *http.Request) {
	locale := extractLocale(r)
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)

	if h.store == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": i18n.T(locale, i18n.MsgInternalError, "media store not configured")})
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

	hintExt := filepath.Ext(origName)
	if hintExt == "" {
		hintExt = ".bin"
	}
	mimeType := media.DetectMIMEType(origName)
	sessionKey := uploadSessionKey(r)

	_, dst, err := h.store.SaveReader(sessionKey, mimeType, file, hintExt)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": i18n.T(locale, i18n.MsgInternalError, "failed to persist upload")})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"path":      dst,
		"mime_type": mimeType,
		"filename":  origName,
	})
}

// uploadSessionKey scopes pre-chat uploads to (tenant, user) so the
// file is tenant-isolated on cleanup but globally addressable by its
// media id from any instance in the ASG.
func uploadSessionKey(r *http.Request) string {
	ctx := r.Context()
	tid := store.TenantIDFromContext(ctx).String()
	uid := store.UserIDFromContext(ctx)
	if uid == "" {
		uid = "anon"
	}
	return fmt.Sprintf("upload:%s:%s", tid, uid)
}
