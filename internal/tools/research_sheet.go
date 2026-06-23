package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/xuri/excelize/v2"

	"github.com/nextlevelbuilder/goclaw/internal/actorheaders"
	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/providers"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

const (
	researchConcurrency = 8
	maxResearchItems    = 120
)

// ResearchSheetTool builds REAL, web-researched data rows for a list of items.
//
// It exists to take the data-sourcing decision away from the model. The agent
// kept "researching" a sheet by recalling values from training instead of
// looking them up — no prompt/skill reliably stopped it. This tool does the
// lookup ITSELF: for each item it runs a web search and an extraction call, so
// the returned values come from live search results, not memory. The model
// just calls this once with the items + columns, then writes the rows to an
// .xlsx and delivers it.
type ResearchSheetTool struct {
	// resolveProvider returns the per-tenant LLM provider+model for the
	// extraction step (same resolver background workers use).
	resolveProvider func(ctx context.Context, tenantID uuid.UUID) (providers.Provider, string, error)
	// tenantStore is needed to attach X-Actor-* headers so llm-service's
	// service-token receiver accepts the extraction calls.
	tenantStore store.TenantStore
	// webSearch is the registered web_search tool, reused for lookups.
	webSearch Tool
	// workspace is the fallback workspace root when ctx carries none.
	workspace string
	// mediaUpload mirrors the delivered .xlsx into the durable media store
	// (S3 on stage/prod) so the download link survives the workspace cleanup
	// cron. When nil, delivery falls back to the local workspace path.
	mediaUpload MediaUploadFunc
}

func NewResearchSheetTool(
	resolveProvider func(ctx context.Context, tenantID uuid.UUID) (providers.Provider, string, error),
	tenantStore store.TenantStore,
	webSearch Tool,
) *ResearchSheetTool {
	return &ResearchSheetTool{resolveProvider: resolveProvider, tenantStore: tenantStore, webSearch: webSearch}
}

// SetWorkspace sets the fallback workspace root for writing the .xlsx when the
// per-run context doesn't carry one.
func (t *ResearchSheetTool) SetWorkspace(ws string) { t.workspace = ws }

// SetMediaUploadFunc enables durable copy of the produced .xlsx to the media
// store (mirrors deliver_file / write_file wiring).
func (t *ResearchSheetTool) SetMediaUploadFunc(fn MediaUploadFunc) { t.mediaUpload = fn }

func (t *ResearchSheetTool) Name() string { return "research_sheet" }

func (t *ResearchSheetTool) Description() string {
	return "Build and DELIVER a real, web-researched spreadsheet (.xlsx) for a list of items. " +
		"Pass the items (row keys, e.g. company/firm names) and the columns to fill; it web-searches EACH item, extracts the column values from live results, writes them to an .xlsx, and sends the download link to the user — all in one call. " +
		"The values come from real search, not memory. You do NOT need to write any code or call deliver_file afterward — this tool produces and delivers the finished file itself."
}

func (t *ResearchSheetTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"items": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": fmt.Sprintf("Row keys to research — one per row (e.g. firm/company names). Max %d.", maxResearchItems),
			},
			"columns": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Column names to fill for each item (e.g. [\"Website\",\"HQ\",\"Stage\",\"Notable Investments\"]). The item name itself is returned too.",
			},
			"context": map[string]any{
				"type":        "string",
				"description": "Optional context to focus the searches (e.g. 'SF Bay Area seed-stage venture capital firm').",
			},
			"filename": map[string]any{
				"type":        "string",
				"description": "Optional output file name for the delivered .xlsx (e.g. 'top_100_vcs.xlsx'). Defaults to 'researched_sheet.xlsx'.",
			},
		},
		"required": []string{"items", "columns"},
	}
}

func (t *ResearchSheetTool) Execute(ctx context.Context, args map[string]any) *Result {
	items := toStringSlice(args["items"])
	columns := toStringSlice(args["columns"])
	topic, _ := args["context"].(string)
	if len(items) == 0 {
		return ErrorResult("items is required (non-empty array of row keys)")
	}
	if len(columns) == 0 {
		return ErrorResult("columns is required (non-empty array of column names)")
	}
	truncated := false
	if len(items) > maxResearchItems {
		items = items[:maxResearchItems]
		truncated = true
	}
	if t.resolveProvider == nil {
		return ErrorResult("research_sheet: no provider resolver configured")
	}

	tenantID := store.TenantIDFromContext(ctx)
	userID := store.UserIDFromContext(ctx)
	prov, model, err := t.resolveProvider(ctx, tenantID)
	if err != nil || prov == nil {
		return ErrorResult(fmt.Sprintf("research_sheet: could not resolve an LLM provider: %v", err))
	}
	if model == "" {
		model = prov.DefaultModel()
	}
	chatCtx := ctx
	if t.tenantStore != nil && tenantID != uuid.Nil && userID != "" {
		chatCtx = actorheaders.Attach(ctx, t.tenantStore, tenantID, userID)
	}

	rows := make([]map[string]string, len(items))
	sem := make(chan struct{}, researchConcurrency)
	var wg sync.WaitGroup
	for i, item := range items {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, item string) {
			defer wg.Done()
			defer func() { <-sem }()
			rows[i] = t.researchOne(ctx, chatCtx, prov, model, item, columns, topic)
		}(i, item)
	}
	wg.Wait()

	// Build the .xlsx from the researched rows and deliver it directly. The
	// model never sees the raw values and never writes the file itself — this is
	// the whole point: it cannot substitute recalled data for the searched data.
	filename := sanitizeXLSXName(strOrEmpty(args["filename"]))
	path, err := t.writeXLSX(ctx, filename, columns, rows)
	if err != nil {
		return ErrorResult(fmt.Sprintf("research_sheet: built the data but failed to write the .xlsx: %v", err))
	}

	deliveredPath := path
	if t.mediaUpload != nil {
		if cachePath := uploadDeliveredToMediaStore(ctx, t.mediaUpload, path); cachePath != "" {
			deliveredPath = cachePath
		}
	}

	filled := 0
	for _, r := range rows {
		for _, c := range columns[1:] { // col 0 is the row key
			if strings.TrimSpace(r[c]) != "" {
				filled++
				break
			}
		}
	}
	msg := fmt.Sprintf("Delivered %s — a researched spreadsheet with %d rows × %d columns, built from live web search (%d/%d rows have at least one researched value). The download link is attached to the chat. Do NOT regenerate this file or 'correct' any values from memory, and do NOT call deliver_file — it is already delivered.", filepath.Base(path), len(rows), len(columns), filled, len(rows))
	if truncated {
		msg += fmt.Sprintf(" Note: only the first %d items were researched — call again for the rest.", maxResearchItems)
	}
	result := SilentResult(msg)
	result.Media = []bus.MediaFile{{Path: deliveredPath, Filename: filepath.Base(path)}}
	if dm := DeliveredMediaFromCtx(ctx); dm != nil {
		dm.Mark(deliveredPath)
	}
	return result
}

// writeXLSX writes the researched rows to an .xlsx in the run workspace and
// returns the absolute path. Row 1 is the column headers; data follows.
func (t *ResearchSheetTool) writeXLSX(ctx context.Context, filename string, columns []string, rows []map[string]string) (string, error) {
	ws := ToolWorkspaceFromCtx(ctx)
	if ws == "" {
		ws = t.workspace
	}
	if ws == "" {
		var err error
		if ws, err = os.MkdirTemp("", "research-sheet-"); err != nil {
			return "", err
		}
	}
	if err := os.MkdirAll(ws, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(ws, filename)

	f := excelize.NewFile()
	defer f.Close()
	const sheet = "Sheet1"
	for j, col := range columns {
		cell, _ := excelize.CoordinatesToCellName(j+1, 1)
		_ = f.SetCellStr(sheet, cell, col)
	}
	for i, row := range rows {
		for j, col := range columns {
			cell, _ := excelize.CoordinatesToCellName(j+1, i+2)
			_ = f.SetCellStr(sheet, cell, row[col])
		}
	}
	if err := f.SaveAs(path); err != nil {
		return "", err
	}
	return path, nil
}

func strOrEmpty(v any) string { s, _ := v.(string); return s }

// sanitizeXLSXName returns a safe, workspace-relative .xlsx filename.
func sanitizeXLSXName(name string) string {
	name = filepath.Base(strings.TrimSpace(name)) // strip any path components
	if name == "" || name == "." || name == "/" {
		return "researched_sheet.xlsx"
	}
	if !strings.HasSuffix(strings.ToLower(name), ".xlsx") {
		name += ".xlsx"
	}
	return name
}

// researchOne searches for one item and extracts the requested columns from the
// results via a single LLM call. Always returns a row (blank values on failure)
// so one bad item never drops a row.
func (t *ResearchSheetTool) researchOne(ctx, chatCtx context.Context, prov providers.Provider, model, item string, columns []string, topic string) map[string]string {
	searchOut := ""
	if t.webSearch != nil {
		q := item
		if topic != "" {
			q += " " + topic
		}
		q += " " + strings.Join(columns, " ")
		if r := t.webSearch.Execute(ctx, map[string]any{"query": q}); r != nil {
			searchOut = r.ForLLM
		}
	}

	sys := "You extract structured data from web search results. Given an item and a list of columns, return ONLY a JSON object whose keys are EXACTLY the column names and whose values come from the search results. Use an empty string \"\" for any column the results don't support — never guess or fill from prior knowledge. No prose, no markdown, no code fences."
	usr := fmt.Sprintf("Item: %s\nColumns: %s\nSearch results:\n%s", item, strings.Join(columns, ", "), searchOut)

	row := map[string]string{}
	resp, err := prov.Chat(chatCtx, providers.ChatRequest{
		Model:    model,
		Messages: []providers.Message{{Role: "system", Content: sys}, {Role: "user", Content: usr}},
		Options:  map[string]any{providers.OptThinkingLevel: "low"},
	})
	if err == nil && resp != nil {
		for k, v := range parseLooseJSONObject(resp.Content) {
			row[k] = v
		}
	}
	// Ensure every requested column key exists; seed the first column with the
	// item name if the model left it blank (the row key should never be empty).
	for j, c := range columns {
		if _, ok := row[c]; !ok {
			row[c] = ""
		}
		if j == 0 && row[c] == "" {
			row[c] = item
		}
	}
	return row
}

func toStringSlice(v any) []string {
	raw, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, e := range raw {
		if s, ok := e.(string); ok {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// parseLooseJSONObject extracts a flat string-map from model output, tolerating
// code fences and surrounding prose by slicing the outermost {...}.
func parseLooseJSONObject(s string) map[string]string {
	out := map[string]string{}
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start < 0 || end <= start {
		return out
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(s[start:end+1]), &raw); err != nil {
		return out
	}
	for k, v := range raw {
		switch val := v.(type) {
		case string:
			out[k] = val
		case nil:
			out[k] = ""
		default:
			b, _ := json.Marshal(val)
			out[k] = string(b)
		}
	}
	return out
}
