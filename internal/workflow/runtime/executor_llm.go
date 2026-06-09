package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/actorheaders"
	"github.com/nextlevelbuilder/goclaw/internal/providers"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// ProviderResolver returns the Provider that should serve a cell for a
// given tenant. Production wiring uses providers.Registry.GetForTenant
// so workflows run on the same per-tenant provider chat sessions use;
// tests pass a fixed-provider closure.
type ProviderResolver func(ctx context.Context, tenantID uuid.UUID) (providers.Provider, error)

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

func (e *LLMCellExecutor) ExecuteCell(ctx context.Context, t CellTask) (CellResult, error) {
	prov := e.provider
	if e.resolveProvider != nil {
		var err error
		prov, err = e.resolveProvider(ctx, t.TenantID)
		if err != nil {
			return CellResult{}, fmt.Errorf("resolve provider for tenant %s: %w", t.TenantID, err)
		}
	}
	if prov == nil {
		return CellResult{}, errors.New("no provider configured")
	}
	user := buildCellUserPrompt(t)

	req := providers.ChatRequest{
		Messages: []providers.Message{
			{Role: "system", Content: cellSystemPrompt},
			{Role: "user", Content: user},
		},
	}
	// Model selection: explicit Model override wins; otherwise fall back
	// to the provider's own default (set by Registry.GetForTenant per
	// the ai_models alias rules). llm-service rejects requests with no
	// model — "model is required" — so this must always end up set.
	switch {
	case e.Model != "":
		req.Model = e.Model
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

	value := normalizeCellValue(resp.Content)
	out := CellResult{Value: value}
	if resp.Usage != nil {
		out.TokensIn = resp.Usage.PromptTokens
		out.TokensOut = resp.Usage.CompletionTokens
	}
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
