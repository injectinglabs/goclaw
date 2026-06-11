package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/actorheaders"
	"github.com/nextlevelbuilder/goclaw/internal/providers"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// CellWebSearch is the minimal interface the cell executor needs to call
// out to a search provider for live data. Kept tiny + here (rather than
// importing internal/tools) to avoid coupling the runtime to the full tool
// registry. The wiring adapts internal/tools.WebSearchTool.Execute into
// this shape — see cmd/gateway_http_wiring.go.
type CellWebSearch interface {
	// Search runs one query and returns the serialized result the
	// executor will hand back to the LLM as the tool message. An empty
	// string is treated as "no results" — the LLM will then fall back to
	// training knowledge.
	Search(ctx context.Context, query string) string
}

// ProviderResolver returns the (Provider, model) pair that should serve
// a cell for a given tenant. Production wiring uses
// providerresolve.ResolveBackgroundProvider so workflows go through the
// SAME provider+model selection background workers do — same
// system_configs / agent.default_model / ai_models alias resolution,
// no duplicate logic. Tests pass a fixed-pair closure.
type ProviderResolver func(ctx context.Context, tenantID uuid.UUID) (providers.Provider, string, error)

// LLMCellExecutor is the production CellExecutor — uses a Provider
// (typically the gateway's "web-agent-api" route, set up with X-Actor-*
// headers so the LLM call bills against the workflow owner's org) to
// resolve one cell at a time.
//
// Prompt structure (kept minimal so providers' tool surface stays free
// for cell content):
//
//	system: "You are a precise data-enrichment assistant. ..."
//	user:   <column.prompt> + serialized row context + type hint
//
// The model is asked to return ONLY the cell value with no prose, no
// markdown, no quotation marks. We strip leading/trailing whitespace +
// surrounding quotes defensively before returning.
type LLMCellExecutor struct {
	// One of provider OR resolveProvider must be non-nil. resolveProvider
	// takes precedence when set so the wiring path can pass a tenant-
	// aware closure without re-allocating LLMCellExecutor per cell.
	provider        providers.Provider
	resolveProvider ProviderResolver
	// tenantStore lets the executor attach X-Actor-User-ID +
	// X-Actor-Org-ID headers to outbound provider.Chat calls so the
	// web-agent-api service-token receiver accepts them. nil → headers
	// skipped (only safe in tests / single-tenant local where the
	// receiver is unauth'd or in api_key mode).
	tenantStore store.TenantStore
	// Optional model override; falls back to provider.DefaultModel().
	Model string
	// Optional live-search hook. When non-nil the executor exposes a
	// single web_search tool to the cell LLM and runs at most ONE tool
	// iteration per cell. Without this, the cell LLM resolves values
	// from training knowledge only — fast but stale. Set via
	// SetWebSearch from the wiring path so existing callers
	// (NewLLMCellExecutor / NewLLMCellExecutorTenant) keep their
	// signatures unchanged.
	webSearch CellWebSearch
}

// SetWebSearch installs a per-cell web_search hook. Passing nil disables
// the feature (default). Called once at startup from the wiring path.
func (e *LLMCellExecutor) SetWebSearch(ws CellWebSearch) {
	e.webSearch = ws
}

// NewLLMCellExecutor wires a fixed Provider — used by tests + single-
// tenant local dev. Production should use NewLLMCellExecutorTenant.
func NewLLMCellExecutor(p providers.Provider) *LLMCellExecutor {
	return &LLMCellExecutor{provider: p}
}

// NewLLMCellExecutorTenant resolves the Provider per cell using the
// callback. The orchestrator passes CellTask.TenantID into the closure
// so workflows use the same tenant-specific provider chat sessions do.
// `ts` is used to attach X-Actor-* headers on outbound chat calls so
// web-agent-api's service-token receiver accepts them; pass nil to skip
// attribution (tests / api_key-mode local dev only).
func NewLLMCellExecutorTenant(resolve ProviderResolver, ts store.TenantStore) *LLMCellExecutor {
	return &LLMCellExecutor{resolveProvider: resolve, tenantStore: ts}
}

const cellSystemPrompt = `You are a precise data-enrichment assistant.
You will be given:
  - A target column name + description (what to put in this cell).
  - A type the cell must conform to.
  - Optional row context: known values from other columns in this row.

Return ONLY the cell value. No prose, no quotes, no markdown, no
explanation. If you cannot determine a reliable value, return the
literal string "" (empty). Do not hallucinate. Verify via web sources
when uncertain.`

// cellSystemPromptWithSearch extends the base prompt with web_search
// usage rules when the executor is wired with a live-search hook. Kept
// terse to avoid bloating per-cell prompt tokens.
const cellSystemPromptWithSearch = cellSystemPrompt + `

You may call the web_search tool to fetch fresh data when the answer
depends on recent events (latest CEO, last funding round, current
price, etc.). If the FIRST search comes back with "(no results)" or
nothing relevant, refine the query and search AGAIN — up to a few
attempts — before giving up. Only return "" (empty) once searches are
genuinely exhausted and your training has nothing either.

Once you have enough to answer, emit the final cell value as plain
text with no further tool calls.

Skip the search entirely for facts your training already covers
(country of HQ, founding year, well-known categorical attributes).
When in doubt about recency, search.`

// cellWebSearchToolDef is the function schema exposed to the cell LLM.
// One required arg, "query", matching the canonical web_search tool's
// surface in internal/tools/web_search.go.
var cellWebSearchToolDef = providers.ToolDefinition{
	Type: "function",
	Function: providers.ToolFunctionSchema{
		Name:        "web_search",
		Description: "Search the web for current information. One call per cell, single broad query.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "Search query — broad and specific to the entity + field you need.",
				},
			},
			"required": []string{"query"},
		},
	},
}

func (e *LLMCellExecutor) ExecuteCell(ctx context.Context, t CellTask) (CellResult, error) {
	var prov providers.Provider
	var resolvedModel string
	if e.resolveProvider != nil {
		var err error
		prov, resolvedModel, err = e.resolveProvider(ctx, t.TenantID)
		if err != nil {
			return CellResult{}, fmt.Errorf("resolve provider for tenant %s: %w", t.TenantID, err)
		}
	} else {
		prov = e.provider
	}
	if prov == nil {
		return CellResult{}, errors.New("no provider configured")
	}
	user := buildCellUserPrompt(t)

	systemPrompt := cellSystemPrompt
	if e.webSearch != nil {
		systemPrompt = cellSystemPromptWithSearch
	}
	req := providers.ChatRequest{
		Messages: []providers.Message{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: user},
		},
	}
	if e.webSearch != nil {
		req.Tools = []providers.ToolDefinition{cellWebSearchToolDef}
	}
	// Model precedence:
	//  1. Explicit Model override on the executor (test / per-deploy
	//     pinning).
	//  2. Model from the resolver (system_configs background.model →
	//     agent.default_model → ai_models alias chain).
	//  3. provider.DefaultModel() as the last-ditch fallback so the
	//     request is never sent with an empty Model — llm-service
	//     rejects "model is required".
	switch {
	case e.Model != "":
		req.Model = e.Model
	case resolvedModel != "":
		req.Model = resolvedModel
	default:
		req.Model = prov.DefaultModel()
	}

	// Attach X-Actor-User-ID + X-Actor-Org-ID so web-agent-api's
	// service-token receiver accepts the call. Without this it returns
	// HTTP 400 "Service-token auth requires X-Actor-User-ID and
	// X-Actor-Org-ID headers" and every cell fails.
	chatCtx := ctx
	if e.tenantStore != nil && t.TenantID != uuid.Nil && t.UserID != "" {
		chatCtx = actorheaders.Attach(ctx, e.tenantStore, t.TenantID, t.UserID)
	}

	resp, err := prov.Chat(chatCtx, req)
	if err != nil {
		return CellResult{}, fmt.Errorf("provider chat: %w", err)
	}
	if resp == nil {
		return CellResult{}, errors.New("provider returned nil response")
	}

	// Token accounting: sum across EVERY LLM call this cell makes (the
	// initial call + every post-search re-prompt). The orchestrator rolls
	// these into the run's total via prog.cellDone, so the user-facing
	// total reflects the real cost including all search round-trips.
	totalIn := 0
	totalOut := 0
	addUsage := func(r *providers.ChatResponse) {
		if r != nil && r.Usage != nil {
			totalIn += r.Usage.PromptTokens
			totalOut += r.Usage.CompletionTokens
		}
	}
	addUsage(resp)

	// Bounded web_search retry loop. While the model keeps issuing a
	// web_search tool call and we haven't hit the cap, run the search and
	// re-prompt. This lets the model search AGAIN when the first query
	// returns "(no results)" — the prompt tells it to refine and retry.
	// Capped so a chatty model can't freeze the wave. The final iteration
	// drops Tools so the model is forced to emit a value.
	if e.webSearch != nil {
		const maxCellSearches = 3
		for searches := 0; searches < maxCellSearches; searches++ {
			if resp.FinishReason != "tool_calls" || len(resp.ToolCalls) == 0 {
				break // model emitted a final value — done.
			}
			var call *providers.ToolCall
			for i := range resp.ToolCalls {
				if resp.ToolCalls[i].Name == "web_search" {
					call = &resp.ToolCalls[i]
					break
				}
			}
			if call == nil {
				break // no web_search call (model hallucinated another tool) — stop.
			}
			query, _ := call.Arguments["query"].(string)
			var searchResult string
			if query != "" {
				searchResult = e.webSearch.Search(chatCtx, query)
			}
			if searchResult == "" {
				searchResult = "(no results)"
			}
			req.Messages = append(req.Messages,
				providers.Message{
					Role:      "assistant",
					Content:   resp.Content,
					ToolCalls: []providers.ToolCall{*call},
				},
				providers.Message{
					Role:       "tool",
					ToolCallID: call.ID,
					Content:    searchResult,
				},
			)
			// On the LAST allowed iteration drop Tools so the model must
			// emit a final value instead of requesting yet another search.
			if searches == maxCellSearches-1 {
				req.Tools = nil
			}
			next, chatErr := prov.Chat(chatCtx, req)
			if chatErr != nil {
				slog.Warn("cell executor: post-search chat failed, using last good content",
					"err", chatErr, "tenant", t.TenantID, "col", t.Column.ID, "search", searches+1)
				break
			}
			if next == nil {
				break
			}
			addUsage(next)
			resp = next
		}
	}

	value := normalizeCellValue(resp.Content)
	out := CellResult{Value: value, TokensIn: totalIn, TokensOut: totalOut}
	return out, nil
}

// buildCellUserPrompt assembles the user-role message from a CellTask.
// Format is fixed so we can assert it in tests.
func buildCellUserPrompt(t CellTask) string {
	var b strings.Builder
	b.WriteString("Column: ")
	b.WriteString(t.Column.Name)
	b.WriteString("\nType: ")
	if t.Column.Type == "" {
		b.WriteString("text")
	} else {
		b.WriteString(t.Column.Type)
	}
	b.WriteString("\nPrompt: ")
	b.WriteString(t.Column.Prompt)
	if len(t.RowContext) > 0 {
		b.WriteString("\n\nRow context (other column values for THIS row):\n")
		// Stable ordering — column id alphabetical — so the system
		// prompt cache stays hot across rows that share schema.
		keys := sortedKeys(t.RowContext)
		for _, k := range keys {
			b.WriteString("  ")
			b.WriteString(k)
			b.WriteString(": ")
			b.WriteString(t.RowContext[k])
			b.WriteString("\n")
		}
	}
	b.WriteString("\nReturn ONLY the value, no prose.")
	return b.String()
}

func normalizeCellValue(s string) string {
	s = strings.TrimSpace(s)
	// Strip surrounding quotes added by overzealous models.
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			s = s[1 : len(s)-1]
		}
	}
	return strings.TrimSpace(s)
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// tiny insertion sort to avoid pulling in sort package + keep
	// allocations down (called per cell, can be thousands).
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	return keys
}
