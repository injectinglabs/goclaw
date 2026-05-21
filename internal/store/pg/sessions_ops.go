package pg

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/providers"
)

func (s *PGSessionStore) TruncateHistory(ctx context.Context, key string, keepLast int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if data, ok := s.cache[sessionCacheKey(ctx, key)]; ok {
		if keepLast <= 0 {
			data.Messages = []providers.Message{}
		} else if len(data.Messages) > keepLast {
			data.Messages = data.Messages[len(data.Messages)-keepLast:]
		}
		data.Updated = time.Now()
	}
}

func (s *PGSessionStore) SetHistory(ctx context.Context, key string, msgs []providers.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if data, ok := s.cache[sessionCacheKey(ctx, key)]; ok {
		data.Messages = msgs
		data.Updated = time.Now()
	}
}

func (s *PGSessionStore) Reset(ctx context.Context, key string) {
	s.mu.Lock()
	if data, ok := s.cache[sessionCacheKey(ctx, key)]; ok {
		data.Messages = []providers.Message{}
		data.Summary = ""
		data.Updated = time.Now()
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()

	// Session not in cache (e.g. after server restart). Clear directly in DB
	// so the next GetOrCreate loads a clean session instead of stale history.
	tid := tenantIDForInsert(ctx)
	if _, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET messages = '[]', summary = '', updated_at = $1
		 WHERE session_key = $2 AND tenant_id = $3`,
		time.Now(), key, tid,
	); err != nil {
		slog.Warn("sessions.reset_db_fallback_failed", "key", key, "error", err)
	}
}

func (s *PGSessionStore) Delete(ctx context.Context, key string) error {
	s.mu.Lock()
	delete(s.cache, sessionCacheKey(ctx, key))
	s.mu.Unlock()

	// Clean up associated media files before deleting from DB.
	if s.OnDelete != nil {
		s.OnDelete(key)
	}

	tid := tenantIDForInsert(ctx)
	_, err := s.db.ExecContext(ctx, "DELETE FROM sessions WHERE session_key = $1 AND tenant_id = $2", key, tid)
	return err
}

// SetLastMessageContent rewrites the content/thinking/status fields on the
// trailing assistant message in the cached message slice. It walks the slice
// backward looking for the most-recent role="assistant" entry, then mutates
// it in place under the store mutex. Other Message fields (Role, CreatedAt,
// MediaRefs, etc.) are left untouched so the placeholder identity survives
// the stream-to-DB partial updates.
//
// Returns an error in two failure modes the caller should treat as "abort
// the flush":
//   - the session is empty (or only has non-assistant messages)
//   - the trailing message is not role="assistant" (race with a tool turn
//     appending in between flush calls — should never happen because chat.go
//     gates flushes behind the per-run streamFlushFn, but defensive)
//
// Save() is NOT invoked here — the agent loop batches the partial write
// behind a debounce, so the caller controls when the cache snapshot lands
// in the DB. This keeps the lock-held window small (~µs).
func (s *PGSessionStore) SetLastMessageContent(ctx context.Context, key string, content, thinking, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, ok := s.cache[sessionCacheKey(ctx, key)]
	if !ok || len(data.Messages) == 0 {
		return errors.New("SetLastMessageContent: session has no messages")
	}
	for i := len(data.Messages) - 1; i >= 0; i-- {
		if data.Messages[i].Role != "assistant" {
			continue
		}
		// Found the trailing assistant. If a tool/user message landed
		// after it, the walk would have hit them first — reject so the
		// caller does not silently rewrite the wrong row.
		if i != len(data.Messages)-1 {
			return errors.New("SetLastMessageContent: trailing message is not assistant")
		}
		data.Messages[i].Content = content
		data.Messages[i].Thinking = thinking
		data.Messages[i].Status = status
		data.Updated = time.Now()
		return nil
	}
	return errors.New("SetLastMessageContent: no assistant message in session")
}

// DropLastStreamingMessage slices off the trailing assistant message when its
// Status is "streaming". flushMessages calls it before appending the real
// finalized turn so the partial placeholder is replaced cleanly, not
// duplicated. No-op (returns nil) if the last message has a different status
// or is not an assistant — this lets multi-iteration turns call it
// idempotently without re-walking on each iteration. Save() is the caller's
// responsibility.
func (s *PGSessionStore) DropLastStreamingMessage(ctx context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, ok := s.cache[sessionCacheKey(ctx, key)]
	if !ok || len(data.Messages) == 0 {
		return nil
	}
	last := &data.Messages[len(data.Messages)-1]
	if last.Role != "assistant" || last.Status != "streaming" {
		return nil
	}
	data.Messages = data.Messages[:len(data.Messages)-1]
	data.Updated = time.Now()
	return nil
}
