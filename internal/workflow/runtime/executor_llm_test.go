package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nextlevelbuilder/goclaw/internal/providers"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// fakeProvider is a stub for providers.Provider used by executor tests.
// Implements only what LLMCellExecutor needs (Chat); ChatStream /
// DefaultModel / Name panic to surface accidental dependencies.
type fakeProvider struct {
	respond func(req providers.ChatRequest) (*providers.ChatResponse, error)
	calls   []providers.ChatRequest
}

func (f *fakeProvider) Chat(_ context.Context, req providers.ChatRequest) (*providers.ChatResponse, error) {
	f.calls = append(f.calls, req)
	return f.respond(req)
}
func (f *fakeProvider) ChatStream(context.Context, providers.ChatRequest, func(providers.StreamChunk)) (*providers.ChatResponse, error) {
	panic("ChatStream not used")
}
func (f *fakeProvider) DefaultModel() string { return "test-model" }
func (f *fakeProvider) Name() string         { return "fake" }

func colExec(id, name, prompt string) store.SheetWorkflowColumn {
	return store.SheetWorkflowColumn{ID: id, Name: name, Prompt: prompt, Type: "text"}
}

func TestExecutor_ReturnsValueAndTokens(t *testing.T) {
	prov := &fakeProvider{respond: func(_ providers.ChatRequest) (*providers.ChatResponse, error) {
		return &providers.ChatResponse{
			Content: "Jane Doe",
			Usage:   &providers.Usage{PromptTokens: 120, CompletionTokens: 4, TotalTokens: 124},
		}, nil
	}}
	exec := NewLLMCellExecutor(prov)
	out, err := exec.ExecuteCell(context.Background(), CellTask{
		Column: colExec("ceo", "CEO", "find ceo"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Value != "Jane Doe" {
		t.Errorf("value: want 'Jane Doe', got %q", out.Value)
	}
	if out.TokensIn != 120 || out.TokensOut != 4 {
		t.Errorf("tokens: want (120,4), got (%d,%d)", out.TokensIn, out.TokensOut)
	}
}

func TestExecutor_StripsSurroundingQuotes(t *testing.T) {
	cases := []struct{ in, want string }{
		{`"value"`, "value"},
		{`'value'`, "value"},
		{`  "with whitespace"  `, "with whitespace"},
		{`no quotes`, "no quotes"},
		{`"`, `"`},        // single char, not stripped
		{`""`, ``},        // empty after stripping
	}
	for _, c := range cases {
		got := normalizeCellValue(c.in)
		if got != c.want {
			t.Errorf("normalize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestExecutor_BuildsPromptWithRowContext(t *testing.T) {
	prov := &fakeProvider{respond: func(_ providers.ChatRequest) (*providers.ChatResponse, error) {
		return &providers.ChatResponse{Content: "x"}, nil
	}}
	exec := NewLLMCellExecutor(prov)
	_, err := exec.ExecuteCell(context.Background(), CellTask{
		Column:     colExec("linkedin", "LinkedIn URL", "find linkedin from name"),
		RowContext: map[string]string{"ceo": "Jane Doe", "company": "Acme"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(prov.calls) != 1 {
		t.Fatalf("want 1 provider call, got %d", len(prov.calls))
	}
	msgs := prov.calls[0].Messages
	if len(msgs) != 2 || msgs[0].Role != "system" || msgs[1].Role != "user" {
		t.Fatalf("expected system+user, got %+v", msgs)
	}
	user := msgs[1].Content
	for _, want := range []string{
		"Column: LinkedIn URL",
		"Type: text",
		"Prompt: find linkedin from name",
		"Row context",
		"ceo: Jane Doe",
		"company: Acme",
	} {
		if !strings.Contains(user, want) {
			t.Errorf("user prompt missing %q.\nFull:\n%s", want, user)
		}
	}
}

func TestExecutor_StableContextKeyOrdering(t *testing.T) {
	// Stable alphabetical ordering of row-context keys ensures the
	// system+user prompts are bit-identical across rows that share
	// the same schema → provider-side prompt cache stays hot.
	prov := &fakeProvider{respond: func(_ providers.ChatRequest) (*providers.ChatResponse, error) {
		return &providers.ChatResponse{Content: "x"}, nil
	}}
	exec := NewLLMCellExecutor(prov)
	_, err := exec.ExecuteCell(context.Background(), CellTask{
		Column:     colExec("b", "B", "p"),
		RowContext: map[string]string{"z": "1", "a": "2", "m": "3"},
	})
	if err != nil {
		t.Fatal(err)
	}
	user := prov.calls[0].Messages[1].Content
	aIdx := strings.Index(user, "a: 2")
	mIdx := strings.Index(user, "m: 3")
	zIdx := strings.Index(user, "z: 1")
	if aIdx < 0 || mIdx < 0 || zIdx < 0 {
		t.Fatalf("missing keys in user prompt:\n%s", user)
	}
	if !(aIdx < mIdx && mIdx < zIdx) {
		t.Errorf("context keys not in alphabetical order: a=%d m=%d z=%d", aIdx, mIdx, zIdx)
	}
}

// whiffSearch is a CellWebSearch that always returns "" — simulates the
// provider missing on every query (rate limit / no relevant hits).
type whiffSearch struct{}

func (whiffSearch) Search(context.Context, string) string { return "" }

// hitSearch returns a fixed snippet — simulates a real search result.
type hitSearch struct{ out string }

func (h hitSearch) Search(context.Context, string) string { return h.out }

// When the model insists on searching a trivial fact and every search
// whiffs, the cell must fall back to a clean training-knowledge call
// instead of coming back blank (the ByteDance "HQ Country"/"Industry"
// regression). Tokens from every call still accumulate.
func TestExecutor_TrainingFallbackWhenSearchWhiffs(t *testing.T) {
	prov := &fakeProvider{respond: func(req providers.ChatRequest) (*providers.ChatResponse, error) {
		if len(req.Tools) > 0 {
			// Model keeps asking to search even for a well-known fact.
			return &providers.ChatResponse{
				FinishReason: "tool_calls",
				ToolCalls: []providers.ToolCall{{
					ID: "c1", Name: "web_search",
					Arguments: map[string]any{"query": "ByteDance HQ country"},
				}},
				Usage: &providers.Usage{PromptTokens: 50, CompletionTokens: 5},
			}, nil
		}
		// No-tools call = the training-knowledge fallback (no useful notes
		// to extract from, so no extraction call precedes it).
		return &providers.ChatResponse{
			Content: "China",
			Usage:   &providers.Usage{PromptTokens: 30, CompletionTokens: 2},
		}, nil
	}}
	exec := NewLLMCellExecutor(prov)
	exec.SetWebSearch(whiffSearch{})
	out, err := exec.ExecuteCell(context.Background(), CellTask{
		Column: colExec("hq", "HQ Country", "country of HQ"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Value != "China" {
		t.Errorf("want 'China' from training fallback, got %q", out.Value)
	}
	if out.TokensIn == 0 || out.TokensOut == 0 {
		t.Errorf("expected accumulated tokens across all calls, got (%d,%d)", out.TokensIn, out.TokensOut)
	}
}

// When search DOES return data, the extraction call turns the snippet
// into a clean value — and the training fallback must NOT fire (value
// already resolved).
func TestExecutor_ExtractsFromRealSearchNotes(t *testing.T) {
	noToolsCalls := 0
	prov := &fakeProvider{respond: func(req providers.ChatRequest) (*providers.ChatResponse, error) {
		if len(req.Tools) > 0 {
			return &providers.ChatResponse{
				FinishReason: "tool_calls",
				ToolCalls: []providers.ToolCall{{
					ID: "c1", Name: "web_search",
					Arguments: map[string]any{"query": "Acme latest funding"},
				}},
				Usage: &providers.Usage{PromptTokens: 40, CompletionTokens: 3},
			}, nil
		}
		noToolsCalls++
		// First no-tools call is the extraction from notes.
		return &providers.ChatResponse{
			Content: "$50M",
			Usage:   &providers.Usage{PromptTokens: 20, CompletionTokens: 2},
		}, nil
	}}
	exec := NewLLMCellExecutor(prov)
	exec.SetWebSearch(hitSearch{out: "Acme raised a $50M Series B led by Foo Capital."})
	out, err := exec.ExecuteCell(context.Background(), CellTask{
		Column: colExec("funding", "Last Funding", "most recent round size"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Value != "$50M" {
		t.Errorf("want '$50M' from extraction, got %q", out.Value)
	}
	// Exactly ONE no-tools call (extraction). The training fallback must
	// not also fire once the value is resolved.
	if noToolsCalls != 1 {
		t.Errorf("expected 1 no-tools call (extraction only), got %d", noToolsCalls)
	}
}

func TestExecutor_NilProvider(t *testing.T) {
	exec := &LLMCellExecutor{provider: nil}
	_, err := exec.ExecuteCell(context.Background(), CellTask{Column: colExec("a", "A", "p")})
	if err == nil {
		t.Fatal("expected error from nil provider")
	}
}

func TestExecutor_ProviderError(t *testing.T) {
	prov := &fakeProvider{respond: func(_ providers.ChatRequest) (*providers.ChatResponse, error) {
		return nil, errors.New("rate limited")
	}}
	exec := NewLLMCellExecutor(prov)
	_, err := exec.ExecuteCell(context.Background(), CellTask{Column: colExec("a", "A", "p")})
	if err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("expected wrapped provider error, got %v", err)
	}
}

func TestExecutor_NilResponseSafe(t *testing.T) {
	prov := &fakeProvider{respond: func(_ providers.ChatRequest) (*providers.ChatResponse, error) {
		return nil, nil
	}}
	exec := NewLLMCellExecutor(prov)
	_, err := exec.ExecuteCell(context.Background(), CellTask{Column: colExec("a", "A", "p")})
	if err == nil {
		t.Fatal("expected error on nil response with nil error")
	}
}
