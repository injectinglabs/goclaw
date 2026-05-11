package agent

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"sync/atomic"

	mcpclient "github.com/mark3labs/mcp-go/client"
	mcpbridge "github.com/nextlevelbuilder/goclaw/internal/mcp"
	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/internal/tools"
)

// getUserMCPTools returns the per-user MCP tool list for this caller. For
// servers that mandate per-user credentials we register a single BridgeTool
// per (server, tool) into the shared registry the first time anyone calls
// the server in this Loop, and wire it with a per-call resolver that
// re-acquires the right pool connection by ctx user_id on every Execute.
// That way two members of the same tenant can run the agent concurrently
// without racing over a single registry entry — registration freezes only
// the tool name / schema, while the MCP HTTP connection (and its baked
// X-Proxy-User header) is looked up fresh per call.
//
// Returns the list of tools the caller actually has credentials for, so
// the chat builder can include them in the LLM tool palette only when the
// caller is authorized.
func (l *Loop) getUserMCPTools(ctx context.Context, userID string) []tools.Tool {
	if len(l.mcpUserCredSrvs) == 0 || l.mcpPool == nil || l.mcpStore == nil || userID == "" {
		if userID == "" && len(l.mcpUserCredSrvs) > 0 {
			slog.Debug("mcp.user_tools_skipped", "reason", "empty_user_id", "servers", len(l.mcpUserCredSrvs))
		}
		return nil
	}

	var userTools []tools.Tool
	reg, _ := l.tools.(*tools.Registry)

	for _, info := range l.mcpUserCredSrvs {
		srv := info.Server

		// Authorization gate: this user must have credentials configured for
		// the server. If not, the tool stays out of their palette entirely.
		uc, err := l.mcpStore.GetUserCredentials(ctx, srv.ID, userID)
		if err != nil || uc == nil || (uc.APIKey == "" && len(uc.Headers) == 0 && len(uc.Env) == 0) {
			continue
		}

		// Bootstrap: ensure tools for this server are in the shared registry.
		// We need an MCP connection to discover the tool list once; we use the
		// current caller's credentials for that bootstrap connection — any
		// authorized user's creds are equally valid for tool discovery and
		// the resulting tool list is identical regardless of caller.
		toolsForSrv, _ := l.ensureUserMCPServerTools(ctx, info, userID, uc, reg)
		userTools = append(userTools, toolsForSrv...)
	}

	if len(userTools) > 0 {
		// Update "mcp" tool group so policy expansion via alsoAllow includes
		// per-user tools. MergeToolGroup is additive — safe across users.
		var names []string
		for _, t := range userTools {
			names = append(names, t.Name())
		}
		l.registry.MergeToolGroup("mcp", names)
		slog.Debug("mcp.user_tools_resolved", "user", userID, "tools", len(userTools))
	}
	return userTools
}

// ensureUserMCPServerTools makes sure the BridgeTools for srv are
// registered in the shared tool registry (once) and returns them. The
// resolver attached to each BridgeTool re-acquires the user-scoped pool
// connection on every Execute so the registered tool is safe for any
// member of the tenant to invoke — its identity is taken from ctx, not
// baked at registration time.
func (l *Loop) ensureUserMCPServerTools(
	ctx context.Context,
	info store.MCPAccessInfo,
	bootstrapUserID string,
	bootstrapCreds *store.MCPUserCredentials,
	reg *tools.Registry,
) ([]tools.Tool, error) {
	srv := info.Server

	// Fast path: tools already registered for this server in this loop.
	if cached, ok := l.mcpServerToolNames.Load(srv.ID); ok {
		names := cached.([]string)
		out := make([]tools.Tool, 0, len(names))
		if reg != nil {
			for _, n := range names {
				if t, exists := reg.Get(n); exists {
					out = append(out, t)
				}
			}
		}
		if len(out) > 0 {
			return out, nil
		}
	}

	// Bootstrap connection — used only to enumerate tools.
	args := mcpbridge.ParseJSONBytesToStringSlice(srv.Args)
	env := mcpbridge.ParseJSONBytesToStringMap(srv.Env)
	if env == nil {
		env = make(map[string]string)
	}
	headers := mcpbridge.ParseJSONBytesToStringMap(srv.Headers)
	if headers == nil {
		headers = make(map[string]string)
	}
	if srv.APIKey != "" && headers["Authorization"] == "" {
		headers["Authorization"] = "Bearer " + srv.APIKey
	}
	if bootstrapCreds.APIKey != "" {
		headers["Authorization"] = "Bearer " + bootstrapCreds.APIKey
	}
	maps.Copy(headers, bootstrapCreds.Headers)
	maps.Copy(env, bootstrapCreds.Env)

	entry, err := l.mcpPool.AcquireUser(ctx, l.tenantID, srv.Name, bootstrapUserID,
		srv.Transport, srv.Command, args, env, srv.URL, headers, srv.TimeoutSec)
	if err != nil {
		slog.Warn("mcp.user_pool_acquire_failed", "server", srv.Name, "user", bootstrapUserID, "error", err)
		return nil, err
	}
	// Drop our reference immediately — the pool keeps the connection alive
	// while it has time-to-live and reuses it for the next AcquireUser call
	// (including ones from the resolver below).
	l.mcpPool.ReleaseUser(mcpbridge.UserPoolKey(l.tenantID, srv.Name, bootstrapUserID))

	resolver := l.makeUserMCPResolver(srv)

	registeredNames := make([]string, 0, len(entry.MCPTools()))
	out := make([]tools.Tool, 0, len(entry.MCPTools()))
	for _, mcpTool := range entry.MCPTools() {
		// clientPtr / connected supplied here are only used as a fallback
		// when resolveClient is unset — for user-scoped tools the resolver
		// always wins. Pass empty placeholders to keep the constructor
		// signature stable and not accidentally pin one user's connection.
		var fallbackClient atomic.Pointer[mcpclient.Client]
		var fallbackConnected atomic.Bool
		fallbackConnected.Store(true)
		bt := mcpbridge.NewBridgeTool(srv.Name, mcpTool, &fallbackClient, srv.ToolPrefix,
			srv.TimeoutSec, &fallbackConnected, srv.ID, l.mcpGrantChecker).
			WithResolveClient(resolver)

		if reg != nil {
			if _, exists := reg.Get(bt.Name()); !exists {
				reg.Register(bt)
			}
			// Already-registered tool with the same name was put there by an
			// earlier turn of this same Loop — reuse it; the resolver attached
			// then is functionally identical (closure captures the same srv
			// metadata + l.mcpStore + l.mcpPool).
			if existing, exists := reg.Get(bt.Name()); exists {
				out = append(out, existing)
			} else {
				out = append(out, bt)
			}
		} else {
			out = append(out, bt)
		}
		registeredNames = append(registeredNames, bt.Name())
	}

	l.mcpServerToolNames.Store(srv.ID, registeredNames)
	return out, nil
}

// makeUserMCPResolver returns a ResolveUserClientFn closure that, on every
// Execute, looks up the caller's per-user MCP credentials and acquires a
// pool connection scoped to (tenant, server, callerUserID). The closure
// captures srv metadata + the Loop's pool / store, so a single registered
// BridgeTool can serve every member of a shared tenant safely.
func (l *Loop) makeUserMCPResolver(srv store.MCPServerData) mcpbridge.ResolveUserClientFn {
	return func(ctx context.Context) (*mcpclient.Client, bool, error) {
		callerUserID := store.UserIDFromContext(ctx)
		if callerUserID == "" {
			return nil, false, fmt.Errorf("missing user context for MCP server %q", srv.Name)
		}

		uc, err := l.mcpStore.GetUserCredentials(ctx, srv.ID, callerUserID)
		if err != nil {
			return nil, false, fmt.Errorf("load user credentials: %w", err)
		}
		if uc == nil || (uc.APIKey == "" && len(uc.Headers) == 0 && len(uc.Env) == 0) {
			return nil, false, fmt.Errorf("no credentials for user on MCP server %q", srv.Name)
		}

		args := mcpbridge.ParseJSONBytesToStringSlice(srv.Args)
		env := mcpbridge.ParseJSONBytesToStringMap(srv.Env)
		if env == nil {
			env = make(map[string]string)
		}
		headers := mcpbridge.ParseJSONBytesToStringMap(srv.Headers)
		if headers == nil {
			headers = make(map[string]string)
		}
		if srv.APIKey != "" && headers["Authorization"] == "" {
			headers["Authorization"] = "Bearer " + srv.APIKey
		}
		if uc.APIKey != "" {
			headers["Authorization"] = "Bearer " + uc.APIKey
		}
		maps.Copy(headers, uc.Headers)
		maps.Copy(env, uc.Env)

		entry, err := l.mcpPool.AcquireUser(ctx, l.tenantID, srv.Name, callerUserID,
			srv.Transport, srv.Command, args, env, srv.URL, headers, srv.TimeoutSec)
		if err != nil {
			return nil, false, fmt.Errorf("acquire pool entry: %w", err)
		}
		// Release immediately — the pool keeps the user connection alive
		// (TTL + LRU eviction) and the next call from this same user reuses
		// it. CallTool runs on the client we hand back before any eviction
		// can race with us.
		defer l.mcpPool.ReleaseUser(mcpbridge.UserPoolKey(l.tenantID, srv.Name, callerUserID))

		client := entry.ClientPtr().Load()
		return client, entry.Connected().Load(), nil
	}
}
