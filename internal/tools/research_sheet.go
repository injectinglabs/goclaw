package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
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
	researchConcurrency = 12
	maxResearchItems    = 150
	// snippetChars caps per-item page text fed to the batched soft-column LLM.
	snippetChars = 1800
	// softLLMBatch is how many items go into one soft-column extraction call.
	softLLMBatch = 12
)

// ResearchSheetTool builds a REAL, web-researched spreadsheet (.xlsx) for a list
// of items — fast and accurate — using SEARCH + full-page EXTRACT, not snippets
// and not recalled data.
//
// For each item it (1) finds the official site via web search, (2) extracts the
// FULL text of the homepage + contact/about/team pages via Tavily /extract, then
// (3) pulls emails by REGEX (domain-filtered, so they're real) and resolves the
// website deterministically. Optional "soft" columns (HQ, stage, focus, …) are
// filled by a single BATCHED LLM pass over the extracted text — a handful of
// calls total, never one-per-row. The .xlsx is written and delivered by the tool
// itself, so the model can't substitute fabricated values.
type ResearchSheetTool struct {
	searchProviders []SearchProvider
	extractor       *tavilyExtractor

	// resolveProvider/tenantStore power the optional batched soft-column LLM pass.
	resolveProvider func(ctx context.Context, tenantID uuid.UUID) (providers.Provider, string, error)
	tenantStore     store.TenantStore

	workspace   string
	mediaUpload MediaUploadFunc
}

// NewResearchSheetTool builds the tool from the web-search config (for discovery
// + the Tavily key used by /extract). Returns nil when Tavily isn't configured —
// /extract is required for accurate page-level research.
func NewResearchSheetTool(cfg WebSearchConfig) *ResearchSheetTool {
	if !cfg.TavilyEnabled || cfg.TavilyAPIKey == "" {
		return nil
	}
	return &ResearchSheetTool{
		searchProviders: buildSearchProviders(cfg),
		extractor:       newTavilyExtractor(cfg.TavilyAPIKey),
	}
}

// SetSoftColumnLLM wires the per-tenant provider resolver + tenant store used to
// fill interpretive columns. Without it, only deterministic columns (item key,
// website, email) are filled; soft columns are left blank.
func (t *ResearchSheetTool) SetSoftColumnLLM(resolve func(ctx context.Context, tenantID uuid.UUID) (providers.Provider, string, error), ts store.TenantStore) {
	t.resolveProvider = resolve
	t.tenantStore = ts
}

func (t *ResearchSheetTool) SetWorkspace(ws string)                { t.workspace = ws }
func (t *ResearchSheetTool) SetMediaUploadFunc(fn MediaUploadFunc) { t.mediaUpload = fn }

func (t *ResearchSheetTool) Name() string { return "research_sheet" }

func (t *ResearchSheetTool) Description() string {
	return "Build and DELIVER a real, web-researched spreadsheet (.xlsx) for a list of items — fast and accurate. " +
		"Pass the items (row keys, e.g. company/firm names) and the columns to fill. For each item it finds the official site, extracts the full text of its homepage + contact/about/team pages, pulls EMAILS by pattern-matching (real, from the page), resolves the website, and fills the rest from the extracted text — then writes the .xlsx and sends the download link to the user. " +
		"Use this for any 'build a sheet/table of N items with researched columns' request. You do NOT write code or call deliver_file afterward; this tool produces and delivers the file itself. Values come from live pages, not memory; cells with nothing found are left blank."
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
				"description": "Column headers, item-name column FIRST (e.g. [\"Firm\",\"Website\",\"Email\",\"HQ\",\"Stage\"]). Columns named like 'Website'/'Email' are filled deterministically; others are extracted from page text.",
			},
			"context": map[string]any{
				"type":        "string",
				"description": "Optional context to disambiguate the search (e.g. 'SF Bay Area seed-stage venture capital firm').",
			},
			"filename": map[string]any{
				"type":        "string",
				"description": "Optional output .xlsx name (e.g. 'top_100_vcs.xlsx'). Defaults to 'researched_sheet.xlsx'.",
			},
		},
		"required": []string{"items", "columns"},
	}
}

// perItem holds what we extracted for one row.
type perItem struct {
	homepage string
	emails   []string
	snippet  string
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
	if t.extractor == nil {
		return ErrorResult("research_sheet: Tavily extract is not configured")
	}
	truncated := false
	if len(items) > maxResearchItems {
		items = items[:maxResearchItems]
		truncated = true
	}

	chain := ResolveWebSearchChain(ctx, t.searchProviders)

	// Phase 1: per-item search + extract + regex (parallel, no LLM).
	data := make([]perItem, len(items))
	sem := make(chan struct{}, researchConcurrency)
	var wg sync.WaitGroup
	for i, item := range items {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, item string) {
			defer wg.Done()
			defer func() { <-sem }()
			data[i] = t.researchOne(ctx, chain, item, topic)
		}(i, item)
	}
	wg.Wait()

	// Classify columns. Column 0 is the row key (item name).
	keyCol := columns[0]
	emailCols, siteCols, softCols := classifyColumns(columns[1:])

	rows := make([]map[string]string, len(items))
	for i, item := range items {
		row := map[string]string{keyCol: item}
		for _, c := range siteCols {
			row[c] = data[i].homepage
		}
		for _, c := range emailCols {
			row[c] = strings.Join(data[i].emails, ", ")
		}
		for _, c := range softCols {
			row[c] = ""
		}
		rows[i] = row
	}

	// Phase 2: batched LLM pass for soft columns (a few calls, not one-per-row).
	if len(softCols) > 0 && t.resolveProvider != nil {
		t.fillSoftColumns(ctx, items, data, softCols, rows)
	}

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

	emailFound := 0
	for _, d := range data {
		if len(d.emails) > 0 {
			emailFound++
		}
	}
	msg := fmt.Sprintf("Delivered %s — a researched spreadsheet with %d rows × %d columns, built from live web pages (found an email for %d/%d rows). The download link is attached to the chat. Do NOT regenerate this file or fill any values from memory, and do NOT call deliver_file — it is already delivered. Blank cells mean nothing was found on the page.", filepath.Base(path), len(rows), len(columns), emailFound, len(rows))
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

// researchOne finds an item's site, extracts its pages, and regexes emails.
func (t *ResearchSheetTool) researchOne(ctx context.Context, chain []SearchProvider, item, topic string) perItem {
	q := item
	if topic != "" {
		q += " " + topic
	}
	q += " official website contact"

	var homepage, host string
	for _, p := range chain {
		res, err := p.Search(ctx, searchParams{Query: q, Count: 3})
		if err != nil || len(res) == 0 {
			continue
		}
		homepage, host = homepageFromURL(res[0].URL)
		if homepage != "" {
			break
		}
	}
	if homepage == "" {
		return perItem{}
	}

	urls := []string{homepage}
	for _, suffix := range []string{"contact", "about", "team", "contact-us"} {
		urls = append(urls, strings.TrimRight(homepage, "/")+"/"+suffix)
	}
	contents, err := t.extractor.Extract(ctx, urls)
	if err != nil || len(contents) == 0 {
		return perItem{homepage: homepage}
	}

	var sb strings.Builder
	for _, c := range contents {
		sb.WriteString(c)
		sb.WriteString("\n")
	}
	full := sb.String()
	return perItem{
		homepage: homepage,
		emails:   extractEmails(full, host),
		snippet:  truncateStr(strings.TrimSpace(full), snippetChars),
	}
}

// fillSoftColumns batches items through the LLM to extract interpretive columns
// from the per-item snippets. Mutates rows in place.
func (t *ResearchSheetTool) fillSoftColumns(ctx context.Context, items []string, data []perItem, softCols []string, rows []map[string]string) {
	tenantID := store.TenantIDFromContext(ctx)
	userID := store.UserIDFromContext(ctx)
	prov, model, err := t.resolveProvider(ctx, tenantID)
	if err != nil || prov == nil {
		return
	}
	if model == "" {
		model = prov.DefaultModel()
	}
	chatCtx := ctx
	if t.tenantStore != nil && tenantID != uuid.Nil && userID != "" {
		chatCtx = actorheaders.Attach(ctx, t.tenantStore, tenantID, userID)
	}

	type batch struct{ start, end int }
	var batches []batch
	for s := 0; s < len(items); s += softLLMBatch {
		e := s + softLLMBatch
		if e > len(items) {
			e = len(items)
		}
		batches = append(batches, batch{s, e})
	}

	var mu sync.Mutex
	sem := make(chan struct{}, researchConcurrency)
	var wg sync.WaitGroup
	for _, b := range batches {
		wg.Add(1)
		sem <- struct{}{}
		go func(b batch) {
			defer wg.Done()
			defer func() { <-sem }()
			extracted := t.softBatch(chatCtx, prov, model, items[b.start:b.end], data[b.start:b.end], softCols)
			mu.Lock()
			for li, vals := range extracted {
				gi := b.start + li
				if gi >= len(rows) {
					continue
				}
				for _, c := range softCols {
					if v := strings.TrimSpace(vals[c]); v != "" {
						rows[gi][c] = v
					}
				}
			}
			mu.Unlock()
		}(b)
	}
	wg.Wait()
}

// softBatch asks the model to extract softCols for each item from its snippet.
// Returns a slice aligned with the input items (index -> col -> value).
func (t *ResearchSheetTool) softBatch(ctx context.Context, prov providers.Provider, model string, items []string, data []perItem, softCols []string) []map[string]string {
	out := make([]map[string]string, len(items))
	for i := range out {
		out[i] = map[string]string{}
	}
	var ub strings.Builder
	ub.WriteString("Columns to extract: " + strings.Join(softCols, ", ") + "\n\n")
	for i, item := range items {
		fmt.Fprintf(&ub, "[%d] %s\n%s\n\n", i, item, data[i].snippet)
	}
	sys := "You extract structured fields from web page text. For each numbered item, return values ONLY for the requested columns, taken strictly from that item's page text. " +
		"Respond with ONLY a JSON array; element i = {\"i\": <index>, " + jsonColHint(softCols) + "}. Use \"\" for any field the text doesn't support — never guess or use prior knowledge. No prose, no code fences."

	resp, err := prov.Chat(ctx, providers.ChatRequest{
		Model:    model,
		Messages: []providers.Message{{Role: "system", Content: sys}, {Role: "user", Content: ub.String()}},
		Options:  map[string]any{providers.OptThinkingLevel: "low"},
	})
	if err != nil || resp == nil {
		return out
	}
	for _, obj := range parseLooseJSONArray(resp.Content) {
		idx, ok := toInt(obj["i"])
		if !ok || idx < 0 || idx >= len(out) {
			continue
		}
		for _, c := range softCols {
			if v, ok := obj[c].(string); ok {
				out[idx][c] = v
			}
		}
	}
	return out
}

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

// --- helpers ---

var emailRe = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)

// junkEmailHosts are noise/tracking domains we never want as a contact email.
var junkEmailHosts = []string{"sentry", "wixpress", "example.", "domain.com", "email.com", "yourcompany", "sentry.io", ".png", ".jpg", ".webp", "wix.com"}

// extractEmails pulls unique emails from text, preferring ones on the site's own
// domain (real contact addresses) and dropping tracking/placeholder noise.
func extractEmails(text, host string) []string {
	root := rootDomain(host)
	seen := map[string]bool{}
	var onDomain, other []string
	for _, m := range emailRe.FindAllString(text, -1) {
		e := strings.ToLower(strings.Trim(m, ".,;:()<>\"' "))
		if seen[e] || isJunkEmail(e) {
			continue
		}
		seen[e] = true
		if root != "" && strings.HasSuffix(e, "@"+root) || (root != "" && strings.Contains(e, "."+root)) {
			onDomain = append(onDomain, e)
		} else {
			other = append(other, e)
		}
	}
	sort.Strings(onDomain)
	if len(onDomain) > 0 {
		return onDomain
	}
	sort.Strings(other)
	if len(other) > 3 {
		other = other[:3]
	}
	return other
}

func isJunkEmail(e string) bool {
	for _, j := range junkEmailHosts {
		if strings.Contains(e, j) {
			return true
		}
	}
	return false
}

// homepageFromURL returns the scheme://host root and the bare host for a URL.
func homepageFromURL(raw string) (homepage, host string) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", ""
	}
	host = strings.TrimPrefix(strings.ToLower(u.Host), "www.")
	scheme := u.Scheme
	if scheme == "" {
		scheme = "https"
	}
	return scheme + "://" + u.Host, host
}

// rootDomain returns the registrable-ish root (last two labels) of a host.
func rootDomain(host string) string {
	host = strings.TrimPrefix(strings.ToLower(host), "www.")
	parts := strings.Split(host, ".")
	if len(parts) < 2 {
		return host
	}
	return strings.Join(parts[len(parts)-2:], ".")
}

func classifyColumns(cols []string) (email, site, soft []string) {
	for _, c := range cols {
		l := strings.ToLower(c)
		switch {
		case strings.Contains(l, "email") || strings.Contains(l, "e-mail"):
			email = append(email, c)
		case strings.Contains(l, "website") || strings.Contains(l, "url") || l == "site" || strings.Contains(l, "domain") || strings.Contains(l, "homepage"):
			site = append(site, c)
		default:
			soft = append(soft, c)
		}
	}
	return
}

func jsonColHint(cols []string) string {
	parts := make([]string, len(cols))
	for i, c := range cols {
		b, _ := json.Marshal(c)
		parts[i] = string(b) + ": \"...\""
	}
	return strings.Join(parts, ", ")
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

func strOrEmpty(v any) string { s, _ := v.(string); return s }

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case string:
		var i int
		if _, err := fmt.Sscanf(strings.TrimSpace(n), "%d", &i); err == nil {
			return i, true
		}
	}
	return 0, false
}

// parseLooseJSONArray extracts a JSON array of objects from model output,
// tolerating code fences / prose by slicing the outermost [...].
func parseLooseJSONArray(s string) []map[string]any {
	start := strings.IndexByte(s, '[')
	end := strings.LastIndexByte(s, ']')
	if start < 0 || end <= start {
		return nil
	}
	var arr []map[string]any
	if err := json.Unmarshal([]byte(s[start:end+1]), &arr); err != nil {
		return nil
	}
	return arr
}

// sanitizeXLSXName returns a safe, workspace-relative .xlsx filename.
func sanitizeXLSXName(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	if name == "" || name == "." || name == "/" {
		return "researched_sheet.xlsx"
	}
	if !strings.HasSuffix(strings.ToLower(name), ".xlsx") {
		name += ".xlsx"
	}
	return name
}
