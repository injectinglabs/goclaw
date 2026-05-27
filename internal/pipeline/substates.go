package pipeline

import (
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/providers"
)

// ContextState: owned by ContextStage, read by ThinkStage.
type ContextState struct {
	ContextFiles   []any  // bootstrap.ContextFile — typed in Phase 2, any avoids circular import
	SkillsSummary  string
	TeamContext    string // team workspace context injected for team runs
	MemorySection  string // L0 auto-injected memory context for system prompt
	Summary        string // session summary for context continuity
	HadBootstrap   bool
	OverheadTokens int // system prompt + context files (accurate via TokenCounter)

	// EffectiveContextWindow is the context window size (in tokens) resolved
	// per-run from the provider/model pair via ModelRegistry. Resolved ONCE in
	// ContextStage and read by PruneStage on every iteration. Zero means "no
	// model-specific data available" and PruneStage falls back to
	// PipelineConfig.ContextWindow.
	//
	// Resolved once per run (not per iteration) to avoid budget skew — if the
	// model somehow changes mid-run a mismatch causes silent truncation loops.
	EffectiveContextWindow int
}

// ThinkState: owned by ThinkStage.
type ThinkState struct {
	LastResponse *providers.ChatResponse
	// TotalUsage sums tokens across every LLM iteration in this run. Used
	// for billing / ai_tasks aggregates where the sum is the right number
	// (each iteration costs separately).
	TotalUsage providers.Usage
	// LastPromptTokens is the prompt_tokens from the FINAL iteration only
	// (last-write-wins). This is the size the upstream provider just sent
	// — and therefore the size that will be re-sent on the next user turn
	// once we append one user message. The session's context-usage UI
	// indicator reads this field, not TotalUsage.PromptTokens, because a
	// 3-iteration tool loop accumulates prompt counts that have no
	// physical meaning for the next-turn context.
	LastPromptTokens int
	TruncRetries     int  // consecutive truncation retries (max 3)
	StreamingActive  bool // true during active stream
}

// PruneState: owned by PruneStage.
type PruneState struct {
	MidLoopCompacted bool // true after first in-loop compaction
	HistoryTokens    int  // last computed history token count
	HistoryBudget    int  // contextWindow * maxHistoryShare
}

// ToolState: owned by ToolStage.
type ToolState struct {
	LoopDetector   any // concrete type toolLoopState lives in agent; Phase 5 defines LoopDetector interface
	TotalToolCalls int
	AsyncToolCalls []string      // tool names that executed async (spawn)
	MediaResults   []MediaResult // media files produced by tools
	Deliverables   []string      // tool output content for team task results
	LoopKilled     bool          // set when loop detector triggers critical
}

// ObserveState: owned by ObserveStage.
type ObserveState struct {
	FinalContent   string // accumulated response text
	FinalThinking  string // reasoning output
	BlockReplies   int
	LastBlockReply string
}

// CompactState: owned by CheckpointStage + MemoryFlushStage.
type CompactState struct {
	CheckpointFlushedMsgs  int
	MemoryFlushedThisCycle bool
	CompactionCount        int
}

// EvolutionState: owned by skill evolution nudge logic.
type EvolutionState struct {
	Nudge70Sent      bool
	Nudge90Sent      bool
	PostscriptSent   bool
	BootstrapWrite   bool // BOOTSTRAP.md write detected
	TeamTaskCreates  int  // team_tasks tool calls
	TeamTaskSpawns   int  // delegate tool calls (spawns)
}

// RunResult is the final output of a pipeline run.
type RunResult struct {
	RunID      string
	Content    string
	Thinking   string
	TotalUsage providers.Usage
	// LastPromptTokens carries the FINAL iteration's prompt_tokens so the
	// agent layer can write it into sessions.last_prompt_tokens without
	// double-counting iterations the way TotalUsage.PromptTokens does.
	LastPromptTokens int
	Iterations       int
	ToolCalls        int
	LoopKilled       bool
	// MaxIterationsReached is true when the run exited because it exhausted
	// Config.MaxIterations (the agent's tool-iteration budget) rather than the
	// model finishing on its own. Surfaced to the client as stop_reason so the
	// UI can offer a "Continue" action that grants a fresh budget.
	MaxIterationsReached bool
	Duration             time.Duration
	AsyncToolCalls   []string
	MediaResults     []MediaResult
	Deliverables     []string
	BlockReplies     int
	LastBlockReply   string
}
