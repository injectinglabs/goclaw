package mcp

import (
	"net/url"
	"os"
	"strings"
	"sync"
)

// mcpAllowedHosts returns the set of hostnames (from GOCLAW_MCP_ALLOWED_HOSTS)
// that bypass the SSRF IP-range check on MCP server URL registration.
// Operator-controlled via process env — NEVER read from DB, prompt, or tool output.
// Intended for docker-network sidecars (e.g. internal MCP connectors) that
// live on private IPs but are trusted by the operator.
var (
	mcpAllowedHostsOnce sync.Once
	mcpAllowedHosts     map[string]bool
)

func loadMCPAllowedHosts() {
	raw := os.Getenv("GOCLAW_MCP_ALLOWED_HOSTS")
	if raw == "" {
		return
	}
	set := make(map[string]bool)
	for _, h := range strings.Split(raw, ",") {
		h = strings.ToLower(strings.TrimSpace(h))
		if h != "" {
			set[h] = true
		}
	}
	if len(set) > 0 {
		mcpAllowedHosts = set
	}
}

// isMCPAllowedHost returns true if rawURL's hostname is operator-allowed.
// All other SSRF checks (scheme, empty host, malformed URL) are NOT bypassed —
// callers still run those via security.Validate for safety, OR the ValidateURL
// wrapper applies a minimal parse-and-scheme check.
func isMCPAllowedHost(rawURL string) bool {
	mcpAllowedHostsOnce.Do(loadMCPAllowedHosts)
	if mcpAllowedHosts == nil {
		return false
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return false
	}
	return mcpAllowedHosts[strings.ToLower(u.Hostname())]
}
