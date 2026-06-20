package pipeline

import (
	"context"
	"testing"

	"github.com/nextlevelbuilder/goclaw/internal/providers"
)

// TestThinkStage_TextTruncationAutoContinues verifies Fix B: a text-only answer
// that stops with FinishReason=="length" is NOT delivered as final — the stage
// accumulates it into ContinuationBuffer and asks to continue (Continue, not
// BreakLoop), WITHOUT persisting any partial assistant/user messages (the
// continuation context is ephemeral, built per-call in step 3).
func TestThinkStage_TextTruncationAutoContinues(t *testing.T) {
	t.Parallel()
	deps := &PipelineDeps{
		Config: PipelineConfig{MaxIterations: 10, MaxTokens: 1000},
		CallLLM: func(_ context.Context, _ *RunState, _ providers.ChatRequest) (*providers.ChatResponse, error) {
			return &providers.ChatResponse{
				Content:      "rows 1-77 of the table",
				FinishReason: "length",
			}, nil
		},
	}
	stage := NewThinkStage(deps)
	state := defaultState()

	if err := stage.Execute(context.Background(), state); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	if stage.Result() != Continue {
		t.Errorf("Result() = %v, want Continue (auto-continue, not deliver truncated)", stage.Result())
	}
	if state.Think.TextContinuations != 1 {
		t.Errorf("TextContinuations = %d, want 1", state.Think.TextContinuations)
	}
	if state.Think.ContinuationBuffer != "rows 1-77 of the table" {
		t.Errorf("ContinuationBuffer = %q, want the truncated chunk", state.Think.ContinuationBuffer)
	}
	// Continuation context must be ephemeral — nothing persisted to history.
	if pending := state.Messages.Pending(); len(pending) != 0 {
		t.Errorf("pending len = %d, want 0 (continuation context is ephemeral)", len(pending))
	}
}

// TestThinkStage_EmptyLengthTruncationDoesNotLoop guards the regression: a
// length-truncated turn with EMPTY content (all-thinking, no answer text) must
// NOT auto-continue (which loops on nothing → empty reply). It BreakLoops so the
// empty-reply rescue runs instead.
func TestThinkStage_EmptyLengthTruncationDoesNotLoop(t *testing.T) {
	t.Parallel()
	deps := &PipelineDeps{
		Config: PipelineConfig{MaxIterations: 10, MaxTokens: 1000},
		CallLLM: func(_ context.Context, _ *RunState, _ providers.ChatRequest) (*providers.ChatResponse, error) {
			return &providers.ChatResponse{Content: "", FinishReason: "length"}, nil
		},
	}
	stage := NewThinkStage(deps)
	state := defaultState()

	if err := stage.Execute(context.Background(), state); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	if stage.Result() != BreakLoop {
		t.Errorf("Result() = %v, want BreakLoop (don't loop on empty)", stage.Result())
	}
	if state.Think.TextContinuations != 0 {
		t.Errorf("TextContinuations = %d, want 0 (no continuation on empty)", state.Think.TextContinuations)
	}
}

// TestThinkStage_TextTruncationThenCleanFinishStitches verifies the second leg:
// after a length-truncation continuation, a clean finish BreakLoops and the
// ObserveStage stitches buffer + final chunk into one complete FinalContent.
func TestThinkStage_TextTruncationThenCleanFinishStitches(t *testing.T) {
	t.Parallel()
	deps := &PipelineDeps{
		Config: PipelineConfig{MaxIterations: 10, MaxTokens: 1000},
		CallLLM: func(_ context.Context, _ *RunState, _ providers.ChatRequest) (*providers.ChatResponse, error) {
			return &providers.ChatResponse{
				Content:      " rows 78-100 of the table",
				FinishReason: "stop",
			}, nil
		},
	}
	think := NewThinkStage(deps)
	observe := NewObserveStage(deps)
	state := defaultState()
	// Simulate one prior truncated chunk already buffered.
	state.Think.ContinuationBuffer = "rows 1-77 of the table"
	state.Think.TextContinuations = 1

	if err := think.Execute(context.Background(), state); err != nil {
		t.Fatalf("think Execute() error: %v", err)
	}
	if think.Result() != BreakLoop {
		t.Errorf("Result() = %v, want BreakLoop (clean finish)", think.Result())
	}
	if err := observe.Execute(context.Background(), state); err != nil {
		t.Fatalf("observe Execute() error: %v", err)
	}
	want := "rows 1-77 of the table rows 78-100 of the table"
	if state.Observe.FinalContent != want {
		t.Errorf("FinalContent = %q, want stitched %q", state.Observe.FinalContent, want)
	}
}

// TestThinkStage_TextTruncationBounded verifies the continuation count is capped
// so a model that keeps truncating can't loop forever — once the cap is hit the
// stage delivers what it has (BreakLoop) instead of continuing.
func TestThinkStage_TextTruncationBounded(t *testing.T) {
	t.Parallel()
	deps := &PipelineDeps{
		Config: PipelineConfig{MaxIterations: 100, MaxTokens: 1000},
		CallLLM: func(_ context.Context, _ *RunState, _ providers.ChatRequest) (*providers.ChatResponse, error) {
			return &providers.ChatResponse{Content: "more", FinishReason: "length"}, nil
		},
	}
	stage := NewThinkStage(deps)
	state := defaultState()
	state.Think.TextContinuations = maxTextContinuations // already at the cap

	if err := stage.Execute(context.Background(), state); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	if stage.Result() != BreakLoop {
		t.Errorf("Result() = %v, want BreakLoop (continuation cap reached)", stage.Result())
	}
}
