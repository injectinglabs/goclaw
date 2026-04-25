// Package secret resolves ${ssm:/path} references against AWS SSM Parameter
// Store. Used to keep gateway secrets (GOCLAW_GATEWAY_TOKEN, encryption key,
// DB DSN, mcp_servers.headers, …) out of .env files on EC2 hosts.
//
// Plain values without the ${ssm:} prefix pass through untouched, so the
// helper is drop-in safe during a staged migration.
package secret

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

const refPrefix = "${ssm:"

// IsRef returns true for values that look like SSM placeholders.
func IsRef(s string) bool {
	return strings.HasPrefix(s, refPrefix) && strings.HasSuffix(s, "}")
}

func extractName(s string) string {
	if !IsRef(s) {
		return ""
	}
	return strings.TrimSpace(s[len(refPrefix) : len(s)-1])
}

// SSMResolver fetches SSM parameters with an in-process TTL cache.
//
// Concurrent callers asking for the same name share one in-flight RPC. After
// a rotation, callers must call Invalidate(name) to force a refresh — there
// is no automatic poll.
type SSMResolver struct {
	client *ssm.Client
	ttl    time.Duration

	mu     sync.Mutex
	cache  map[string]cacheEntry
	flight map[string]chan resolveResult
}

type cacheEntry struct {
	value     string
	expiresAt time.Time
}

type resolveResult struct {
	value string
	err   error
}

// SSMOption configures the resolver.
type SSMOption func(*SSMResolver)

// WithTTL overrides the default 5-minute cache TTL. Zero or negative
// disables caching entirely (useful in tests).
func WithTTL(d time.Duration) SSMOption {
	return func(r *SSMResolver) { r.ttl = d }
}

// NewSSMResolver constructs a resolver using the default AWS SDK credential
// chain (EC2 IMDS, env vars, shared config). Region defaults to AWS_REGION
// or us-east-1.
func NewSSMResolver(ctx context.Context, opts ...SSMOption) (*SSMResolver, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("secret: load AWS config: %w", err)
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	return NewSSMResolverWithClient(ssm.NewFromConfig(cfg), opts...), nil
}

// NewSSMResolverWithClient lets callers inject a pre-built ssm.Client.
func NewSSMResolverWithClient(c *ssm.Client, opts ...SSMOption) *SSMResolver {
	r := &SSMResolver{
		client: c,
		ttl:    5 * time.Minute,
		cache:  make(map[string]cacheEntry),
		flight: make(map[string]chan resolveResult),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Resolve returns the value for a placeholder or passes a literal through.
// Cache hits return without an RPC; misses coalesce with any in-flight call
// for the same name.
func (r *SSMResolver) Resolve(ctx context.Context, value string) (string, error) {
	name := extractName(value)
	if name == "" {
		return value, nil
	}
	return r.fetch(ctx, name)
}

// ResolveMap drains a map[string]string in place, replacing any ${ssm:...}
// values with their resolved versions. Returns the first error if any.
//
// Used to resolve mcp_servers.headers JSONB before passing it to the MCP
// transport — the resolved map carries no SSM-references downstream.
func (r *SSMResolver) ResolveMap(ctx context.Context, m map[string]string) error {
	for k, v := range m {
		if !IsRef(v) {
			continue
		}
		resolved, err := r.Resolve(ctx, v)
		if err != nil {
			return fmt.Errorf("resolve %s: %w", k, err)
		}
		m[k] = resolved
	}
	return nil
}

// Invalidate drops a single name from the cache. Subsequent Resolve calls
// will re-fetch from SSM. Call this from rotation handlers.
func (r *SSMResolver) Invalidate(name string) {
	r.mu.Lock()
	delete(r.cache, name)
	r.mu.Unlock()
}

func (r *SSMResolver) fetch(ctx context.Context, name string) (string, error) {
	r.mu.Lock()
	if entry, ok := r.cache[name]; ok && time.Now().Before(entry.expiresAt) {
		r.mu.Unlock()
		return entry.value, nil
	}
	if ch, ok := r.flight[name]; ok {
		r.mu.Unlock()
		res := <-ch
		return res.value, res.err
	}
	ch := make(chan resolveResult, 1)
	r.flight[name] = ch
	r.mu.Unlock()

	value, err := r.fetchUncached(ctx, name)

	r.mu.Lock()
	delete(r.flight, name)
	if err == nil && r.ttl > 0 {
		r.cache[name] = cacheEntry{value: value, expiresAt: time.Now().Add(r.ttl)}
	}
	r.mu.Unlock()

	res := resolveResult{value: value, err: err}
	ch <- res
	close(ch)
	return value, err
}

func (r *SSMResolver) fetchUncached(ctx context.Context, name string) (string, error) {
	out, err := r.client.GetParameter(ctx, &ssm.GetParameterInput{
		Name:           aws.String(name),
		WithDecryption: aws.Bool(true),
	})
	if err != nil {
		return "", fmt.Errorf("ssm GetParameter %q: %w", name, err)
	}
	if out.Parameter == nil || out.Parameter.Value == nil {
		return "", fmt.Errorf("ssm parameter %q has no value", name)
	}
	return *out.Parameter.Value, nil
}
