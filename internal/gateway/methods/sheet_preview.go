package methods

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

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

const (
	enrichBatchSize   = 25 // rows per LLM call — small enough to fit + parse cleanly
	enrichConcurrency = 6  // parallel batches (bounded so we don't trip provider rate limits)
	enrichBatchRetry  = 3  // attempts per batch before giving up (transient errors / rate limits)
)

// fillColumns fills each requested column for every row. Rows are split into
// batches run CONCURRENTLY (bounded) so a 250-row fill takes ~one batch's
// latency instead of one giant serial call — and each small batch parses
// reliably. Partial success is kept (blank cells for any failed batch); only an
// all-batches-failed result errors out.
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

	// Compact context line per row from the EXISTING (non-enrich) columns so the
	// model can identify each entity.
	ctxLine := make([]string, len(grid.Rows))
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
		ctxLine[ri] = strings.Join(parts, " | ")
	}

	for _, col := range cols {
		ci := colIdx[col]
		type batch struct{ start, end int }
		var batches []batch
		for s := 0; s < len(grid.Rows); s += enrichBatchSize {
			e := s + enrichBatchSize
			if e > len(grid.Rows) {
				e = len(grid.Rows)
			}
			batches = append(batches, batch{s, e})
		}

		errs := make([]error, len(batches))
		var wg sync.WaitGroup
		sem := make(chan struct{}, enrichConcurrency)
		for bi, b := range batches {
			wg.Add(1)
			go func(bi int, b batch) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				vals, err := m.fillBatch(ctx, provider, model, col, ctxLine[b.start:b.end], userID, tenantID)
				if err != nil {
					errs[bi] = err
					return
				}
				for j := 0; j < b.end-b.start; j++ {
					if j < len(vals) {
						grid.Rows[b.start+j][ci] = vals[j]
					}
				}
			}(bi, b)
		}
		wg.Wait()

		failed := 0
		var firstErr error
		for _, e := range errs {
			if e != nil {
				failed++
				if firstErr == nil {
					firstErr = e
				}
			}
		}
		if failed == len(batches) && firstErr != nil {
			return fmt.Errorf("fill %q: %w", col, firstErr)
		}
		if failed > 0 {
			slog.Warn("sheet.enrich partial", "col", col, "failed_batches", failed, "of", len(batches))
		}
	}
	return nil
}

// fillBatch asks the model for one column's value across a slice of rows and
// returns a JSON array of strings (one per row, in order). It retries on
// transient errors / rate limits and tolerates a truncated reply by salvaging
// the values that did parse — so a single bad response degrades gracefully
// (some rows blank) instead of dropping the whole batch.
func (m *SheetPreviewMethods) fillBatch(ctx context.Context, provider providers.Provider, model, col string, lines []string, userID string, tenantID uuid.UUID) ([]string, error) {
	var b strings.Builder
	for i, l := range lines {
		fmt.Fprintf(&b, "%d) %s\n", i+1, l)
	}
	sys := "You fill ONE spreadsheet column using your knowledge. Return ONLY a JSON array of strings — exactly one value per row, in the SAME ORDER as the rows given. Use \"\" when genuinely unknown. No prose, no markdown fences, no keys — just [\"v1\",\"v2\",...]."
	user := fmt.Sprintf("There are %d rows. For each, give the value of the column %q.\n\nRows:\n%s\nReturn a JSON array of exactly %d strings now.",
		len(lines), col, b.String(), len(lines))

	// Budget output tokens to the batch size (≈160 tok/value) so values don't
	// truncate, clamped to a sane window.
	maxTok := 160 * len(lines)
	if maxTok < 2048 {
		maxTok = 2048
	}
	if maxTok > 16000 {
		maxTok = 16000
	}

	var lastErr error
	for attempt := 0; attempt < enrichBatchRetry; attempt++ {
		if attempt > 0 {
			// Brief backoff before retrying (rate limits / transient 5xx).
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * 400 * time.Millisecond):
			}
		}
		resp, err := provider.Chat(ctx, providers.ChatRequest{
			Messages: []providers.Message{{Role: "system", Content: sys}, {Role: "user", Content: user}},
			Model:    model,
			Options: map[string]any{
				providers.OptThinkingLevel: "off",
				providers.OptMaxTokens:     maxTok,
				providers.OptUserID:        userID,
				providers.OptTenantID:      tenantID.String(),
			},
		})
		if err != nil {
			lastErr = err
			continue
		}
		vals, perr := parseStringArrayLenient(resp.Content)
		if perr != nil {
			slog.Warn("sheet.enrich batch parse failed", "col", col, "attempt", attempt, "finish", resp.FinishReason, "preview", clipStr(resp.Content, 160))
			lastErr = fmt.Errorf("parse (finish=%s): %w", resp.FinishReason, perr)
			continue
		}
		// A truncated reply (finish=length) may yield fewer values than rows —
		// keep what we got; the caller leaves the rest blank rather than retrying
		// the whole batch forever.
		return vals, nil
	}
	return nil, lastErr
}

// parseStringArrayLenient parses a JSON array of strings, salvaging as many
// leading elements as possible when the array is truncated (e.g. the model hit
// the token limit mid-array). Returns an error only when nothing usable parses.
func parseStringArrayLenient(raw string) ([]string, error) {
	s := stripFences(raw)
	var vals []string
	if err := json.Unmarshal([]byte(s), &vals); err == nil {
		return vals, nil
	}
	// Stream-decode element by element; stop at the first broken/incomplete one.
	dec := json.NewDecoder(strings.NewReader(s))
	// Advance to the opening '['.
	for {
		t, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("no JSON array found: %w", err)
		}
		if d, ok := t.(json.Delim); ok && d == '[' {
			break
		}
	}
	for dec.More() {
		var v string
		if err := dec.Decode(&v); err != nil {
			break // truncated mid-element — keep what we have
		}
		vals = append(vals, v)
	}
	if len(vals) == 0 {
		return nil, fmt.Errorf("no values parsed")
	}
	return vals, nil
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
