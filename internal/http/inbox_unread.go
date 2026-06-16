package http

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// InboxHandler serves unread-count lookups for the browser extension's
// background badge (v0: interval polling). It asks composio-mcp for the
// acting user's unread email count per provider. Real-time push (Composio
// triggers → webhook) is a later v1; this is a cheap on-demand poll.
type InboxHandler struct {
	composioURL string // base URL of composio-mcp, e.g. http://composio-mcp:9300
	httpClient  *http.Client
}

// NewInboxHandler constructs the inbox unread-count handler.
func NewInboxHandler(composioURL string) *InboxHandler {
	return &InboxHandler{
		composioURL: composioURL,
		httpClient:  &http.Client{Timeout: 12 * time.Second},
	}
}

// RegisterRoutes registers the inbox routes on the given mux.
func (h *InboxHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/inbox/unread", requireAuth("", h.handleUnread))
}

// handleUnread returns {"gmail": N, "total": N} for the authenticated user.
// Best-effort: a provider that errors (not connected, auth expired) contributes
// 0 rather than failing the whole request, so the badge degrades gracefully.
func (h *InboxHandler) handleUnread(w http.ResponseWriter, r *http.Request) {
	userID := store.UserIDFromContext(r.Context())
	if userID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "no user"})
		return
	}

	gmail := h.gmailUnread(r.Context(), userID)

	writeJSON(w, http.StatusOK, map[string]any{
		"gmail": gmail,
		"total": gmail,
	})
}

// gmailUnread returns the user's unread INBOX count via composio GMAIL_FETCH_EMAILS.
// Returns 0 on any error (not connected, auth expired, parse failure) — the
// caller treats this as "nothing to show".
func (h *InboxHandler) gmailUnread(ctx context.Context, userID string) int {
	text, err := h.callComposio(ctx, userID, "GMAIL_FETCH_EMAILS", map[string]any{
		"query":       "is:unread in:inbox",
		"max_results": 25,
	})
	if err != nil {
		slog.Debug("inbox.gmail_unread_failed", "user", userID, "err", err.Error())
		return 0
	}
	n := extractGmailCount(text)
	slog.Debug("inbox.gmail_unread", "user", userID, "count", n)
	return n
}

// callComposio invokes a composio-mcp tool for the given user and returns the
// first text content block. Mirrors the JSON-RPC tools/call shape used by the
// sheet writer; composio-mcp resolves the connected account from X-Proxy-User.
func (h *InboxHandler) callComposio(ctx context.Context, userID, tool string, args map[string]any) (string, error) {
	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params":  map[string]any{"name": tool, "arguments": args},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", h.composioURL+"/mcp", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Proxy-User", userID)

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("call: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("composio %s status %s", tool, resp.Status)
	}

	var rpc struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rpc); err != nil {
		return "", fmt.Errorf("decode: %w", err)
	}
	if rpc.Error != nil {
		return "", fmt.Errorf("rpc error: %s", rpc.Error.Message)
	}
	if len(rpc.Result.Content) == 0 {
		return "", fmt.Errorf("empty content")
	}
	if rpc.Result.IsError {
		return "", fmt.Errorf("tool error: %s", clip(rpc.Result.Content[0].Text, 200))
	}
	return rpc.Result.Content[0].Text, nil
}

// clip truncates s to at most n bytes for safe error logging.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// extractGmailCount pulls the unread count out of a GMAIL_FETCH_EMAILS result.
// GMAIL_FETCH_EMAILS wraps Gmail users.messages.list, which returns
// resultSizeEstimate + a messages array. Composio may nest the payload under
// "data", so we search both levels and fall back to counting messages. Defensive
// by design — the exact envelope is verified on staging via the debug log above.
func extractGmailCount(text string) int {
	var m map[string]any
	if err := json.Unmarshal([]byte(text), &m); err != nil {
		return 0
	}
	if n, ok := countFromMap(m); ok {
		return n
	}
	if data, ok := m["data"].(map[string]any); ok {
		if n, ok := countFromMap(data); ok {
			return n
		}
	}
	return 0
}

func countFromMap(m map[string]any) (int, bool) {
	if v, ok := m["resultSizeEstimate"].(float64); ok {
		return int(v), true
	}
	if msgs, ok := m["messages"].([]any); ok {
		return len(msgs), true
	}
	return 0, false
}
