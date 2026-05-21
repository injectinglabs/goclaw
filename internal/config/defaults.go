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

	// DefaultSummarizerModelAlias is the model alias used when invoking the
	// summarizer LLM call. Summarization is a trivial task and should not run on
	// the agent's primary model (which may be expensive). "fast" is resolved via
	// the LLM service alias registry to a cheap, fast-tier model.
	DefaultSummarizerModelAlias = "fast"
)
