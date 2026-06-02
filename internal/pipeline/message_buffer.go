package pipeline

import "github.com/nextlevelbuilder/goclaw/internal/providers"

// MessageBuffer wraps the message list with append/replace semantics.
// Sequential pipeline guarantees only one stage writes at a time — no mutex needed.
type MessageBuffer struct {
	system    providers.Message   // system prompt (rebuilt by ContextStage)
	history   []providers.Message // conversation history
	pending   []providers.Message // new messages this iteration (flushed at checkpoint)
	ephemeral []providers.Message // included in All() for LLM, NOT persisted via FlushPending
}

// NewMessageBuffer creates a buffer with the given system message.
func NewMessageBuffer(system providers.Message) *MessageBuffer {
	return &MessageBuffer{system: system}
}

// All returns system + history + pending + ephemeral as a single slice for
// LLM calls. Ephemeral messages are visible to the model but NOT persisted
// via FlushPending — used by the in-pipeline subagent barrier to inject a
// `[System Message]` containing spawned-children results into the model's
// context without writing it back as a synthetic user turn in history
// (which would break UI grouping of the assistant turn on page reload).
func (mb *MessageBuffer) All() []providers.Message {
	out := make([]providers.Message, 0, 1+len(mb.history)+len(mb.pending)+len(mb.ephemeral))
	out = append(out, mb.system)
	out = append(out, mb.history...)
	out = append(out, mb.pending...)
	out = append(out, mb.ephemeral...)
	return out
}

// System returns the system message.
func (mb *MessageBuffer) System() providers.Message { return mb.system }

// SetSystem replaces the system message (ContextStage rebuilds it).
func (mb *MessageBuffer) SetSystem(msg providers.Message) { mb.system = msg }

// History returns conversation history (read-only view).
func (mb *MessageBuffer) History() []providers.Message { return mb.history }

// SetHistory replaces history (used when loading from session store).
func (mb *MessageBuffer) SetHistory(msgs []providers.Message) { mb.history = msgs }

// AppendPending adds a new message to the pending buffer.
func (mb *MessageBuffer) AppendPending(msg providers.Message) {
	mb.pending = append(mb.pending, msg)
}

// AppendEphemeral adds a transient message visible to the LLM (via All())
// but excluded from FlushPending — so it never reaches the session store.
// Used by the barrier hook to inject `[System Message]` blocks into the
// model's context for synthesis passes without dirtying conversation
// history with a fake `role:user` turn.
func (mb *MessageBuffer) AppendEphemeral(msg providers.Message) {
	mb.ephemeral = append(mb.ephemeral, msg)
}

// Pending returns pending messages (read-only view).
func (mb *MessageBuffer) Pending() []providers.Message { return mb.pending }

// FlushPending moves pending messages to history and returns them.
func (mb *MessageBuffer) FlushPending() []providers.Message {
	flushed := mb.pending
	mb.history = append(mb.history, mb.pending...)
	mb.pending = nil
	return flushed
}

// ReplaceHistory replaces history after compaction.
func (mb *MessageBuffer) ReplaceHistory(msgs []providers.Message) {
	mb.history = msgs
	mb.pending = nil // compaction absorbs pending
}

// HistoryLen returns history count (excludes system + pending).
func (mb *MessageBuffer) HistoryLen() int { return len(mb.history) }

// TotalLen returns total message count including system.
func (mb *MessageBuffer) TotalLen() int {
	return 1 + len(mb.history) + len(mb.pending)
}
