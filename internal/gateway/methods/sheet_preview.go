package methods

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/nextlevelbuilder/goclaw/internal/gateway"
	"github.com/nextlevelbuilder/goclaw/internal/i18n"
	"github.com/nextlevelbuilder/goclaw/internal/sheetgrid"
	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/pkg/protocol"
)

// SheetPreviewMethods serves the `sheet.preview` RPC: parse a delivered
// spreadsheet file in the user's workspace/data dir into a JSON grid the chat
// UI renders as an interactive table. Read path of the interactive-spreadsheet
// feature; the file path comes from the message's MediaRef (a `/v1/files/...`
// link goclaw itself produced), so we validate it stays within the workspace /
// data-dir boundary (same bounds as the file-serving handler).
type SheetPreviewMethods struct {
	workspace string
	dataDir   string
}

// NewSheetPreviewMethods wires the preview RPC with the workspace + data-dir
// roots used for path-boundary validation.
func NewSheetPreviewMethods(workspace, dataDir string) *SheetPreviewMethods {
	return &SheetPreviewMethods{workspace: workspace, dataDir: dataDir}
}

func (m *SheetPreviewMethods) Register(router *gateway.MethodRouter) {
	router.Register(protocol.MethodSheetPreview, m.handlePreview)
}

// handlePreview parses { "path": "<file or /v1/files URL>" } → a sheetgrid.Grid.
//
// Errors:
//   - INVALID_REQUEST — path missing, traversal, or outside allowed dirs
//   - NOT_FOUND       — file missing or not a readable spreadsheet
func (m *SheetPreviewMethods) handlePreview(ctx context.Context, client *gateway.Client, req *protocol.RequestFrame) {
	locale := store.LocaleFromContext(ctx)

	var params struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil || params.Path == "" {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, i18n.T(locale, i18n.MsgRequired, "path")))
		return
	}

	abs, err := m.resolve(params.Path)
	if err != nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, err.Error()))
		return
	}

	grid, err := sheetgrid.Parse(abs)
	if err != nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrNotFound, "could not read spreadsheet"))
		return
	}
	client.SendResponse(protocol.NewOKResponse(req.ID, grid))
}

// resolve normalizes a client-supplied path (accepts a raw absolute path or a
// "/v1/files/<abspath>?ft=..." download URL), then enforces the workspace /
// data-dir boundary and rejects traversal — mirroring internal/http/files.go.
func (m *SheetPreviewMethods) resolve(raw string) (string, error) {
	p := raw
	// Accept the signed download-URL form the client already has.
	if i := strings.Index(p, "/v1/files/"); i >= 0 {
		p = p[i+len("/v1/files/"):]
	}
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	if dec, derr := url.QueryUnescape(p); derr == nil {
		p = dec
	}
	if strings.Contains(p, "..") {
		return "", fmt.Errorf("invalid path")
	}

	var abs string
	if len(p) >= 2 && p[1] == ':' { // Windows drive letter (C:/...)
		abs = filepath.Clean(p)
	} else {
		abs = filepath.Clean("/" + strings.TrimPrefix(p, "/"))
	}

	sep := string(filepath.Separator)
	inWorkspace := m.workspace != "" && (strings.HasPrefix(abs, m.workspace+sep) || abs == m.workspace)
	inDataDir := m.dataDir != "" && (strings.HasPrefix(abs, m.dataDir+sep) || abs == m.dataDir)
	if !inWorkspace && !inDataDir {
		return "", fmt.Errorf("path outside allowed directories")
	}
	return abs, nil
}
