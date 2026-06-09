package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nextlevelbuilder/goclaw/internal/providers"
)

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
	provider providers.Provider
	// Optional model override; falls back to provider.DefaultModel().
	Model string
}

func NewLLMCellExecutor(p providers.Provider) *LLMCellExecutor {
	return &LLMCellExecutor{provider: p}
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
	if e.provider == nil {
		return CellResult{}, errors.New("no provider configured")
	}
	user := buildCellUserPrompt(t)

	req := providers.ChatRequest{
		Messages: []providers.Message{
			{Role: "system", Content: cellSystemPrompt},
			{Role: "user", Content: user},
		},
	}
	if e.Model != "" {
		req.Model = e.Model
	}

	resp, err := e.provider.Chat(ctx, req)
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
