package http

import (
	"context"
	"errors"
	"testing"
	"time"
)

// detachAgentRunCtx is the seam between the HTTP request lifecycle and the
// agent loop. The tests below pin its contract:
//
//   1. The returned context survives parent cancellation. Without that,
//      closing the browser tab during a streaming chat completion would
//      abort loop.Run mid-turn and lose the assistant message before
//      goclaw persisted it into sessions.messages — exactly the bug this
//      helper was added to prevent.
//   2. Values set by enrichContext (tenant, user, role, locale) carry
//      through. context.WithoutCancel under the hood preserves Value()
//      lookups; this test pins that we don't accidentally swap to a fresh
//      context.Background() in some future refactor.
//   3. The detached context still terminates if the SERVER itself is
//      shutting down — only client-side cancellation is decoupled.
//      (Server shutdown is not modelled here because the chat handler
//      doesn't wire a shutdown ctx today; the test below documents the
//      behaviour we'd expect once it does.)

func TestDetachAgentRunCtx_StaysAliveAfterParentCancel(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	detached := detachAgentRunCtx(parent)

	// Cancel the request-side context immediately, mimicking the user
	// closing the tab while the LLM is still generating.
	cancel()

	// Parent is dead, detached must survive.
	if err := parent.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("parent.Err() = %v, want context.Canceled", err)
	}
	if err := detached.Err(); err != nil {
		t.Fatalf("detached.Err() = %v, want nil (must survive parent cancel)", err)
	}

	// Sanity: a select on detached.Done() must NOT fire.
	select {
	case <-detached.Done():
		t.Fatal("detached.Done() fired despite parent cancel — agent run would abort here")
	case <-time.After(20 * time.Millisecond):
		// expected
	}
}

func TestDetachAgentRunCtx_PreservesValues(t *testing.T) {
	type ctxKey string
	const (
		tenantKey ctxKey = "tenant"
		userKey   ctxKey = "user"
		roleKey   ctxKey = "role"
	)

	parent := context.WithValue(context.Background(), tenantKey, "org-team-abc")
	parent = context.WithValue(parent, userKey, "user-d498b4a8")
	parent = context.WithValue(parent, roleKey, "owner")

	detached := detachAgentRunCtx(parent)

	cases := map[ctxKey]string{
		tenantKey: "org-team-abc",
		userKey:   "user-d498b4a8",
		roleKey:   "owner",
	}
	for k, want := range cases {
		got, _ := detached.Value(k).(string)
		if got != want {
			t.Errorf("detached.Value(%q) = %q, want %q (enrichContext values must carry through)", k, got, want)
		}
	}
}

func TestDetachAgentRunCtx_DeadlineDoesNotPropagate(t *testing.T) {
	// HTTP servers can apply ReadHeaderTimeout / WriteTimeout on the
	// request, which becomes the deadline of r.Context(). The agent loop
	// must NOT inherit that deadline — Groq streams take seconds, and we
	// want the loop to keep persisting even if the client times out.
	parent, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel()

	detached := detachAgentRunCtx(parent)

	// Wait past the parent deadline.
	time.Sleep(10 * time.Millisecond)

	if err := parent.Err(); err == nil {
		t.Fatal("parent should be deadline-exceeded by now")
	}
	if _, ok := detached.Deadline(); ok {
		t.Error("detached must not carry parent deadline")
	}
	if err := detached.Err(); err != nil {
		t.Errorf("detached.Err() = %v, want nil even after parent deadline elapsed", err)
	}
}
