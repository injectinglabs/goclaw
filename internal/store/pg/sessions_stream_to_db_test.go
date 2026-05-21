package pg

import (
	"context"
	"testing"

	"github.com/nextlevelbuilder/goclaw/internal/providers"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// TestSetLastMessageContent_DropLastStreamingMessage_RoundTrip exercises the
// stream-to-DB cache-mutation path without touching the DB: a warm cache
// already contains [user, assistant{streaming}], SetLastMessageContent
// rewrites the placeholder fields multiple times (debounce simulation), then
// DropLastStreamingMessage slices it off so the next AddMessage lands at the
// right index.
//
// The PGSessionStore.db field is left nil — the methods under test never
// issue queries (they only mutate the in-memory cache). This is the same
// trick TestReset_WarmCache_ClearsHistory uses.
func TestSetLastMessageContent_DropLastStreamingMessage_RoundTrip(t *testing.T) {
	s := &PGSessionStore{cache: make(map[string]*store.SessionData)}
	ctx := context.Background()
	key := "agent:abc:ws:direct:test"
	cacheKey := sessionCacheKey(ctx, key)

	// Pre-populate cache to simulate chat.go eager-AddMessage(user) +
	// eager-AddMessage(streaming placeholder).
	s.cache[cacheKey] = &store.SessionData{
		Key: key,
		Messages: []providers.Message{
			{Role: "user", Content: "hi"},
			{Role: "assistant", Content: "", Status: "streaming"},
		},
	}

	// First debounce flush: partial content lands.
	if err := s.SetLastMessageContent(ctx, key, "hello", "", "streaming"); err != nil {
		t.Fatalf("SetLastMessageContent#1: %v", err)
	}
	got := s.cache[cacheKey].Messages
	if len(got) != 2 {
		t.Fatalf("message count after first flush = %d, want 2", len(got))
	}
	if got[1].Content != "hello" || got[1].Status != "streaming" {
		t.Errorf("after first flush: got %+v", got[1])
	}
	if got[1].Role != "assistant" {
		t.Errorf("role drifted after content update: %q", got[1].Role)
	}

	// Second debounce flush: thinking field gets a value too. Verify we
	// replace, not append.
	if err := s.SetLastMessageContent(ctx, key, "hello world", "thinking-out-loud", "streaming"); err != nil {
		t.Fatalf("SetLastMessageContent#2: %v", err)
	}
	got = s.cache[cacheKey].Messages
	if got[1].Content != "hello world" || got[1].Thinking != "thinking-out-loud" {
		t.Errorf("after second flush: got %+v", got[1])
	}

	// Drop: flushMessages calls this before appending finalized turn.
	if err := s.DropLastStreamingMessage(ctx, key); err != nil {
		t.Fatalf("DropLastStreamingMessage: %v", err)
	}
	got = s.cache[cacheKey].Messages
	if len(got) != 1 {
		t.Fatalf("after drop: message count = %d, want 1", len(got))
	}
	if got[0].Role != "user" {
		t.Errorf("survived message should be user, got %q", got[0].Role)
	}

	// Drop is idempotent: second call should no-op (last message is user,
	// not a streaming assistant).
	if err := s.DropLastStreamingMessage(ctx, key); err != nil {
		t.Errorf("second DropLastStreamingMessage should no-op nil, got %v", err)
	}
	if got := s.cache[cacheKey].Messages; len(got) != 1 {
		t.Errorf("second drop mutated cache: len=%d", len(got))
	}
}

// TestSetLastMessageContent_RejectsNonAssistantTail verifies the safety
// net: if the trailing message is not an assistant (e.g. a tool result
// landed unexpectedly), the method refuses to silently rewrite the wrong
// row.
func TestSetLastMessageContent_RejectsNonAssistantTail(t *testing.T) {
	s := &PGSessionStore{cache: make(map[string]*store.SessionData)}
	ctx := context.Background()
	key := "agent:abc:ws:direct:test"
	cacheKey := sessionCacheKey(ctx, key)

	s.cache[cacheKey] = &store.SessionData{
		Key: key,
		Messages: []providers.Message{
			{Role: "assistant", Content: "earlier reply"},
			{Role: "user", Content: "follow-up"},
		},
	}

	if err := s.SetLastMessageContent(ctx, key, "new content", "", "streaming"); err == nil {
		t.Fatal("expected error when trailing message is not assistant, got nil")
	}
	// And the slice must not have been mutated.
	if s.cache[cacheKey].Messages[0].Content != "earlier reply" {
		t.Errorf("cache was mutated despite error: %+v", s.cache[cacheKey].Messages)
	}
}

// TestDropLastStreamingMessage_NoOpOnNonStreaming verifies the drop only
// fires for a trailing assistant with Status="streaming" — other shapes
// are left alone so multi-iteration turns can call it idempotently.
func TestDropLastStreamingMessage_NoOpOnNonStreaming(t *testing.T) {
	s := &PGSessionStore{cache: make(map[string]*store.SessionData)}
	ctx := context.Background()
	key := "agent:abc:ws:direct:test"
	cacheKey := sessionCacheKey(ctx, key)

	s.cache[cacheKey] = &store.SessionData{
		Key: key,
		Messages: []providers.Message{
			{Role: "user", Content: "hi"},
			{Role: "assistant", Content: "completed reply"}, // Status == ""
		},
	}

	if err := s.DropLastStreamingMessage(ctx, key); err != nil {
		t.Fatalf("DropLastStreamingMessage: %v", err)
	}
	if got := s.cache[cacheKey].Messages; len(got) != 2 {
		t.Errorf("completed assistant should not be dropped: len=%d", len(got))
	}
}
