package pg

import (
	"context"
	"log/slog"
	"time"
)

func (s *PGSessionStore) AccumulateTokens(ctx context.Context, key string, input, output int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if data, ok := s.cache[sessionCacheKey(ctx, key)]; ok {
		data.InputTokens += input
		data.OutputTokens += output
	}
}

func (s *PGSessionStore) IncrementCompaction(ctx context.Context, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if data, ok := s.cache[sessionCacheKey(ctx, key)]; ok {
		data.CompactionCount++
	}
}

func (s *PGSessionStore) GetCompactionCount(ctx context.Context, key string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if data, ok := s.cache[sessionCacheKey(ctx, key)]; ok {
		return data.CompactionCount
	}
	return 0
}

func (s *PGSessionStore) GetMemoryFlushCompactionCount(ctx context.Context, key string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if data, ok := s.cache[sessionCacheKey(ctx, key)]; ok {
		return data.MemoryFlushCompactionCount
	}
	return -1
}

func (s *PGSessionStore) SetMemoryFlushDone(ctx context.Context, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if data, ok := s.cache[sessionCacheKey(ctx, key)]; ok {
		data.MemoryFlushCompactionCount = data.CompactionCount
		data.MemoryFlushAt = time.Now().UnixMilli()
	}
}

func (s *PGSessionStore) SetSpawnInfo(ctx context.Context, key, spawnedBy string, depth int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if data, ok := s.cache[sessionCacheKey(ctx, key)]; ok {
		data.SpawnedBy = spawnedBy
		data.SpawnDepth = depth
	}
}

func (s *PGSessionStore) SetContextWindow(ctx context.Context, key string, cw int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if data, ok := s.cache[sessionCacheKey(ctx, key)]; ok {
		data.ContextWindow = cw
	}
}

func (s *PGSessionStore) GetContextWindow(ctx context.Context, key string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if data, ok := s.cache[sessionCacheKey(ctx, key)]; ok {
		return data.ContextWindow
	}
	return 0
}

func (s *PGSessionStore) SetLastPromptTokens(ctx context.Context, key string, tokens, msgCount int) {
	s.mu.Lock()
	if data, ok := s.cache[sessionCacheKey(ctx, key)]; ok {
		data.LastPromptTokens = tokens
		data.LastMessageCount = msgCount
	}
	s.mu.Unlock()

	// Persist to DB so sessions.list returns the real prompt-token count
	// from the upstream provider instead of the byte-based fallback. Saving
	// here (not in the periodic Save() snapshot) keeps the column tight to
	// the moment the provider reports usage, so a goclaw restart between
	// run.completed and the next sidebar refresh doesn't lose the value.
	// Errors are tolerated: failed persistence falls back to the legacy
	// LENGTH(messages)/4 estimate in sessions_list.go, the cache copy is
	// still good for in-process readers.
	tid := tenantIDForInsert(ctx)
	if _, err := s.db.ExecContext(ctx,
		`UPDATE sessions
		    SET last_prompt_tokens = $1, last_message_count = $2
		  WHERE session_key = $3 AND tenant_id = $4`,
		tokens, msgCount, key, tid,
	); err != nil {
		slog.Warn("sessions: persist last_prompt_tokens failed",
			"session_key", key, "tenant_id", tid, "err", err)
	}
}

func (s *PGSessionStore) GetLastPromptTokens(ctx context.Context, key string) (int, int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if data, ok := s.cache[sessionCacheKey(ctx, key)]; ok {
		return data.LastPromptTokens, data.LastMessageCount
	}
	return 0, 0
}
