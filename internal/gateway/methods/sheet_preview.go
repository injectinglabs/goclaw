package methods

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/nextlevelbuilder/goclaw/internal/gateway"
	"github.com/nextlevelbuilder/goclaw/internal/i18n"
	"github.com/nextlevelbuilder/goclaw/internal/providerresolve"
	"github.com/nextlevelbuilder/goclaw/internal/providers"
	"github.com/nextlevelbuilder/goclaw/internal/sheetgrid"
	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/pkg/protocol"
)

// SheetPreviewMethods serves the interactive-spreadsheet RPCs:
//   - sheet.preview — parse a delivered .xlsx/.csv → grid JSON (render)
//   - sheet.save    — rewrite the file from an edited grid (no LLM; in place)
//   - sheet.enrich  — fill newly-added empty columns via one LLM call, in place
//
// The file path comes from the message's MediaRef (a `/v1/files/...` link
// goclaw produced); we validate it stays within the workspace/data-dir bounds
// (same as the file-serving handler). registry+sysConfigs power sheet.enrich.
type SheetPreviewMethods struct {
	workspace  string
	dataDir    string
	registry   *providers.Registry
	sysConfigs store.SystemConfigStore
}

// NewSheetPreviewMethods wires the RPCs. registry+sysConfigs are optional —
// without them sheet.enrich reports "not configured" (preview/save still work).
func NewSheetPreviewMethods(workspace, dataDir string, registry *providers.Registry, sysConfigs store.SystemConfigStore) *SheetPreviewMethods {
	return &SheetPreviewMethods{workspace: workspace, dataDir: dataDir, registry: registry, sysConfigs: sysConfigs}
}

func (m *SheetPreviewMethods) Register(router *gateway.MethodRouter) {
	router.Register(protocol.MethodSheetPreview, m.handlePreview)
	router.Register(protocol.MethodSheetSave, m.handleSave)
	router.Register(protocol.MethodSheetEnrich, m.handleEnrich)
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

// gridParams is the edited grid the client sends for save/enrich.
type gridParams struct {
	Path          string     `json:"path"`
	Sheet         string     `json:"sheet"`
	Columns       []string   `json:"columns"`
	Rows          [][]string `json:"rows"`
	EnrichColumns []string   `json:"enrich_columns,omitempty"` // headers to fill (enrich only)
}

func (p gridParams) grid() *sheetgrid.Grid {
	return &sheetgrid.Grid{Sheet: p.Sheet, Columns: p.Columns, Rows: p.Rows}
}

// handleSave rewrites the spreadsheet file in place from the edited grid — no
// LLM, no chat turn. Used for cell edits / rename / delete-column / add-row.
func (m *SheetPreviewMethods) handleSave(ctx context.Context, client *gateway.Client, req *protocol.RequestFrame) {
	locale := store.LocaleFromContext(ctx)
	var p gridParams
	if err := json.Unmarshal(req.Params, &p); err != nil || p.Path == "" {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, i18n.T(locale, i18n.MsgRequired, "path")))
		return
	}
	abs, err := m.resolve(p.Path)
	if err != nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, err.Error()))
		return
	}
	if err := sheetgrid.Write(abs, p.grid()); err != nil {
		slog.Warn("sheet.save write failed", "err", err)
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInternal, "could not save spreadsheet"))
		return
	}
	client.SendResponse(protocol.NewOKResponse(req.ID, map[string]any{"ok": true}))
}

// handleEnrich fills the named empty columns for every row via ONE LLM call
// (model knowledge — founding year, CEO, sector, etc.), writes the file in
// place, and returns the enriched grid so the client updates the same view.
func (m *SheetPreviewMethods) handleEnrich(ctx context.Context, client *gateway.Client, req *protocol.RequestFrame) {
	var p gridParams
	if err := json.Unmarshal(req.Params, &p); err != nil || p.Path == "" || len(p.EnrichColumns) == 0 {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, "path and enrich_columns are required"))
		return
	}
	abs, err := m.resolve(p.Path)
	if err != nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, err.Error()))
		return
	}

	// Identity comes from the WS client, NOT the request context: the router
	// scopes per-request ctx with TenantID/TenantSlug but does NOT propagate the
	// user id, so store.UserIDFromContext is empty here. Use client.* instead.
	tenantID := client.TenantID()
	userID := client.UserID()
	// Use the SAME provider+model as the chat agent ("llm-service"/"default"),
	// which is known to work wherever chat works; fall back to the background
	// resolver only if that isn't registered. Mirrors the draft-reply path.
	model := "default"
	provider, perr := m.registry.GetForTenant(tenantID, "llm-service")
	if perr != nil || provider == nil {
		provider, model = providerresolve.ResolveBackgroundProvider(ctx, tenantID, m.registry, m.sysConfigs)
	}
	if provider == nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInternal, "no model available to fill columns"))
		return
	}
	// Service-token attribution (X-Actor-User-ID/Org-ID) — llm-service 400s
	// without both. Same as the agent loop / draft-reply path.
	org := client.TenantSlug()
	if org == "" {
		org = tenantID.String()
	}
	actor := map[string]string{"X-Actor-User-ID": userID, "X-Actor-Org-ID": org}
	ctx = providers.WithActorHeaders(ctx, actor)

	grid := p.grid()
	if err := m.fillColumns(ctx, provider, model, grid, p.EnrichColumns, userID, tenantID); err != nil {
		slog.Warn("sheet.enrich failed", "err", err)
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInternal, "could not fill columns: "+err.Error()))
		return
	}
	if err := sheetgrid.Write(abs, grid); err != nil {
		slog.Warn("sheet.enrich write failed", "err", err)
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInternal, "could not save spreadsheet"))
		return
	}
	client.SendResponse(protocol.NewOKResponse(req.ID, grid))
}

// fillColumns fills each requested column for every row with ONE LLM call per
// column. A per-column JSON ARRAY OF STRINGS is far more compact (and far less
// likely to truncate) than one giant array-of-objects across all columns, and
// each call gets a generous max_tokens so 100s of rows fit.
func (m *SheetPreviewMethods) fillColumns(ctx context.Context, provider providers.Provider, model string, grid *sheetgrid.Grid, cols []string, userID string, tenantID uuid.UUID) error {
	// Ensure every requested column exists + rows are padded to width.
	colIdx := map[string]int{}
	for i, c := range grid.Columns {
		colIdx[c] = i
	}
	enrichSet := map[string]bool{}
	for _, c := range cols {
		enrichSet[c] = true
		if _, ok := colIdx[c]; !ok {
			colIdx[c] = len(grid.Columns)
			grid.Columns = append(grid.Columns, c)
		}
	}
	width := len(grid.Columns)
	for ri := range grid.Rows {
		for len(grid.Rows[ri]) < width {
			grid.Rows[ri] = append(grid.Rows[ri], "")
		}
	}

	// One compact context line per row, built from the EXISTING (non-enrich)
	// columns so the model can identify each entity.
	var ctxLines strings.Builder
	for ri, row := range grid.Rows {
		var parts []string
		for ci, cn := range grid.Columns {
			if enrichSet[cn] {
				continue
			}
			if ci < len(row) && strings.TrimSpace(row[ci]) != "" {
				parts = append(parts, cn+"="+row[ci])
			}
		}
		fmt.Fprintf(&ctxLines, "%d) %s\n", ri+1, strings.Join(parts, " | "))
	}

	for _, col := range cols {
		sys := "You fill ONE spreadsheet column using your knowledge. Return ONLY a JSON array of strings — exactly one value per row, in the SAME ORDER as the rows given. Use \"\" when genuinely unknown. No prose, no markdown fences, no keys — just [\"v1\",\"v2\",...]."
		user := fmt.Sprintf("There are %d rows. For each, give the value of the column %q.\n\nRows:\n%s\nReturn a JSON array of exactly %d strings now.",
			len(grid.Rows), col, ctxLines.String(), len(grid.Rows))

		resp, err := provider.Chat(ctx, providers.ChatRequest{
			Messages: []providers.Message{{Role: "system", Content: sys}, {Role: "user", Content: user}},
			Model:    model,
			Options: map[string]any{
				providers.OptThinkingLevel: "off",
				providers.OptMaxTokens:     16000, // room for hundreds of short values
				providers.OptUserID:        userID,
				providers.OptTenantID:      tenantID.String(),
			},
		})
		if err != nil {
			return fmt.Errorf("llm fill %q: %w", col, err)
		}
		var vals []string
		if err := json.Unmarshal([]byte(stripFences(resp.Content)), &vals); err != nil {
			slog.Warn("sheet.enrich parse failed", "col", col, "finish", resp.FinishReason, "preview", clipStr(resp.Content, 200))
			return fmt.Errorf("parse fill for %q (finish=%s): %w", col, resp.FinishReason, err)
		}
		ci := colIdx[col]
		for ri := range grid.Rows {
			if ri < len(vals) {
				grid.Rows[ri][ci] = vals[ri]
			}
		}
	}
	return nil
}

func clipStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// stripFences removes a leading ```json / ``` fence and trailing ``` if the
// model wrapped its JSON despite instructions.
func stripFences(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}
