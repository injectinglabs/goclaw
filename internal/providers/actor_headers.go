package providers

import "context"

// actorHeadersKey scopes the actor headers map inside a request context.
// Unexported on purpose — only WithActorHeaders / actorHeadersFromCtx
// should ever touch the value.
type actorHeadersKey struct{}

// WithActorHeaders attaches a static set of HTTP header values that the
// OpenAI-compatible provider's outbound HTTP client will copy verbatim
// onto every request made on this context.
//
// Used by the agent loop to forward an actor identity to downstream
// services that need to attribute the call (`X-Actor-User-ID`,
// `X-Actor-Org-ID`, ...) without leaking the actor's own credentials.
// The headers travel together with the request context, so they apply
// to ALL outbound calls made during a single agent turn — initial chat
// request, tool-driven follow-ups, retries — without each call site
// re-plumbing the actor.
//
// Pass nil or empty map to clear. Re-keys via context, not on the
// provider struct, because the provider is shared across many actors
// while the headers must vary per agent turn.
func WithActorHeaders(ctx context.Context, headers map[string]string) context.Context {
	if len(headers) == 0 {
		return ctx
	}
	// Defensive copy: callers that build the map inline and keep
	// mutating it must not be able to retroactively change headers on
	// requests that already entered the HTTP layer.
	clone := make(map[string]string, len(headers))
	for k, v := range headers {
		if k == "" || v == "" {
			continue
		}
		clone[k] = v
	}
	if len(clone) == 0 {
		return ctx
	}
	return context.WithValue(ctx, actorHeadersKey{}, clone)
}

// actorHeadersFromCtx reads the actor headers map attached via
// WithActorHeaders. Returns nil if none was attached. The returned map
// must NOT be mutated by callers — providers read keys from it during
// request construction.
func actorHeadersFromCtx(ctx context.Context) map[string]string {
	if ctx == nil {
		return nil
	}
	v, _ := ctx.Value(actorHeadersKey{}).(map[string]string)
	return v
}

// ActorHeadersFromCtx is the exported accessor for the same context map
// `WithActorHeaders` writes. Lets callers outside the providers package
// (e.g. internal/mcp) forward goclaw's outbound actor identity to
// downstream service-token receivers without reinventing the
// external_org_id → slug resolution that loop_context.go already does.
//
// The returned map shares storage with the caller's context value; do
// NOT mutate it.
func ActorHeadersFromCtx(ctx context.Context) map[string]string {
	return actorHeadersFromCtx(ctx)
}
