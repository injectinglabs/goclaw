package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/actorheaders"
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
}

func NewResearchSheetTool(
	resolveProvider func(ctx context.Context, tenantID uuid.UUID) (providers.Provider, string, error),
	tenantStore store.TenantStore,
	webSearch Tool,
) *ResearchSheetTool {
	return &ResearchSheetTool{resolveProvider: resolveProvider, tenantStore: tenantStore, webSearch: webSearch}
}

func (t *ResearchSheetTool) Name() string { return "research_sheet" }

func (t *ResearchSheetTool) Description() string {
	return "Build REAL, web-researched data rows for a list of items — the data engine for a researched spreadsheet/table. " +
		"Pass the items (row keys, e.g. company/firm names) and the columns to fill; it web-searches EACH item and extracts the column values from live results, returning finished rows as JSON. " +
		"Use this instead of filling columns from memory: the values come from real search, not recall. After it returns, write the rows to an .xlsx and deliver_file."
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

	payload := map[string]any{"columns": columns, "rows": rows}
	buf, _ := json.Marshal(payload)
	msg := fmt.Sprintf("Researched %d items via live web search. The rows below are real (search-derived) data — write them straight to an .xlsx (row 1 = columns) and deliver_file. Do NOT regenerate or 'correct' values from memory.\n\n%s", len(items), string(buf))
	if truncated {
		msg += fmt.Sprintf("\n\n(note: only the first %d items were researched — call again for the rest.)", maxResearchItems)
	}
	return NewResult(msg)
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
