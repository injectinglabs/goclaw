package config

// Default agent configuration values.
// These are the single source of truth — all fallback/default logic should reference these
// instead of hardcoding numeric literals.
const (
	DefaultContextWindow   = 200000
	DefaultMaxTokens       = 8192
	DefaultMaxMessageChars = 32000
	DefaultMaxIterations   = 30
	DefaultTemperature     = 0.7
	DefaultHistoryShare    = 0.85

	// DefaultKeepLastMessages is the default number of trailing messages preserved
	// across summarization/compaction. Industry default is higher than the legacy
	// value of 4 — Microsoft Agent Framework defaults minimumPreserved to 32 groups,
	// OpenCode keeps a 40K-token cushion, Claude Code keeps the last tool-call
	// results. 16 strikes a balance between context retention and token usage.
	DefaultKeepLastMessages = 16

	// DefaultSummarizerModelAlias controls which model the summarizer LLM call
	// uses. Empty (the default) means: reuse the agent's primary model. This
	// keeps quality consistent with the rest of the turn at the price of a
	// little extra cost vs a dedicated cheap summarizer.
	//
	// Operators who want to flip summarization to a cheaper/faster model can
	// set `CompactionConfig.SummarizerModel = "fast"` (or any other alias the
	// LLM service resolves) — no code change, just config. This is the
	// industry-recommended hybrid: simple default, configurable when cost
	// becomes a concern.
	DefaultSummarizerModelAlias = ""
)
