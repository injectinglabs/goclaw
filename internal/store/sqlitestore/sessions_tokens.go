//go:build sqlite || sqliteonly

package sqlitestore

import (
	"context"
	"log/slog"
	"time"
)

func (s *SQLiteSessionStore) AccumulateTokens(ctx context.Context, key string, input, output int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if data, ok := s.cache[sessionCacheKey(ctx, key)]; ok {
		data.InputTokens += input
		data.OutputTokens += output
	}
}

func (s *SQLiteSessionStore) IncrementCompaction(ctx context.Context, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if data, ok := s.cache[sessionCacheKey(ctx, key)]; ok {
		data.CompactionCount++
	}
}

func (s *SQLiteSessionStore) GetCompactionCount(ctx context.Context, key string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if data, ok := s.cache[sessionCacheKey(ctx, key)]; ok {
		return data.CompactionCount
	}
	return 0
}

func (s *SQLiteSessionStore) GetMemoryFlushCompactionCount(ctx context.Context, key string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if data, ok := s.cache[sessionCacheKey(ctx, key)]; ok {
		return data.MemoryFlushCompactionCount
	}
	return -1
}

func (s *SQLiteSessionStore) SetMemoryFlushDone(ctx context.Context, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if data, ok := s.cache[sessionCacheKey(ctx, key)]; ok {
		data.MemoryFlushCompactionCount = data.CompactionCount
		data.MemoryFlushAt = time.Now().UnixMilli()
	}
}

func (s *SQLiteSessionStore) SetSpawnInfo(ctx context.Context, key, spawnedBy string, depth int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if data, ok := s.cache[sessionCacheKey(ctx, key)]; ok {
		data.SpawnedBy = spawnedBy
		data.SpawnDepth = depth
	}
}

func (s *SQLiteSessionStore) SetContextWindow(ctx context.Context, key string, cw int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if data, ok := s.cache[sessionCacheKey(ctx, key)]; ok {
		data.ContextWindow = cw
	}
}

func (s *SQLiteSessionStore) GetContextWindow(ctx context.Context, key string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if data, ok := s.cache[sessionCacheKey(ctx, key)]; ok {
		return data.ContextWindow
	}
	return 0
}

func (s *SQLiteSessionStore) SetLastPromptTokens(ctx context.Context, key string, tokens, msgCount int) {
	s.mu.Lock()
	if data, ok := s.cache[sessionCacheKey(ctx, key)]; ok {
		data.LastPromptTokens = tokens
		data.LastMessageCount = msgCount
	}
	s.mu.Unlock()

	// Mirror of the Postgres path: persist immediately so the SELECT in
	// sessions_list.go picks up the value across goclaw restarts. SQLite
	// only matters in tests + local dev, but keeping the two stores in
	// sync avoids surprises when someone runs the same migration scenario
	// against an SQLite fixture.
	tid := tenantIDForInsert(ctx)
	if _, err := s.db.ExecContext(ctx,
		`UPDATE sessions
		    SET last_prompt_tokens = ?, last_message_count = ?
		  WHERE session_key = ? AND tenant_id = ?`,
		tokens, msgCount, key, tid,
	); err != nil {
		slog.Warn("sessions: persist last_prompt_tokens failed (sqlite)",
			"session_key", key, "tenant_id", tid, "err", err)
	}
}

func (s *SQLiteSessionStore) GetLastPromptTokens(ctx context.Context, key string) (int, int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if data, ok := s.cache[sessionCacheKey(ctx, key)]; ok {
		return data.LastPromptTokens, data.LastMessageCount
	}
	return 0, 0
}
