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
	outlook := h.outlookUnread(r.Context(), userID)

	writeJSON(w, http.StatusOK, map[string]any{
		"gmail":   gmail,
		"outlook": outlook,
		"total":   gmail + outlook,
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
		slog.Info("inbox.gmail_unread_failed", "user", userID, "err", err.Error())
		return 0
	}
	n, ok := extractCount(text)
	// Log the envelope SHAPE (top-level keys only, never email content) so the
	// parser can be verified/fixed from logs without leaking PII.
	slog.Info("inbox.gmail_unread", "user", userID, "count", n, "parsed", ok, "shape", jsonShape(text))
	return n
}

// outlookUnread returns the user's unread inbox count via composio
// OUTLOOK_LIST_MESSAGES (Microsoft Graph: filter isRead eq false). Returns 0 on
// any error. Same best-effort contract as gmailUnread.
func (h *InboxHandler) outlookUnread(ctx context.Context, userID string) int {
	// Don't rely on a server-side OData filter param (Composio's arg name for it
	// is unreliable — passing it returned all messages). Fetch recent inbox
	// messages and count unread client-side via each message's isRead field,
	// which Microsoft Graph always returns. Capped at the page size (a large
	// unread count just shows "99+" anyway).
	text, err := h.callComposio(ctx, userID, "OUTLOOK_LIST_MESSAGES", map[string]any{
		"folder": "inbox",
		"top":    50,
	})
	if err != nil {
		slog.Info("inbox.outlook_unread_failed", "user", userID, "err", err.Error())
		return 0
	}
	n, ok := countUnreadOutlook(text)
	slog.Info("inbox.outlook_unread", "user", userID, "count", n, "by_isread", ok, "shape", jsonShape(text))
	return n
}

// countUnreadOutlook counts messages with isRead==false in an
// OUTLOOK_LIST_MESSAGES result. Returns (count, sawIsRead); sawIsRead is false
// when no item exposed an isRead field (so the caller can tell "0 unread" apart
// from "couldn't parse" and avoid a bogus page-size count).
func countUnreadOutlook(text string) (int, bool) {
	var m map[string]any
	if err := json.Unmarshal([]byte(text), &m); err != nil {
		return 0, false
	}
	arr := outlookMessageArray(m)
	count, sawIsRead := 0, false
	for _, it := range arr {
		msg, ok := it.(map[string]any)
		if !ok {
			continue
		}
		// Microsoft Graph uses "isRead"; tolerate a snake_case passthrough too.
		for _, key := range []string{"isRead", "is_read"} {
			if v, ok := msg[key].(bool); ok {
				sawIsRead = true
				if !v {
					count++
				}
				break
			}
		}
	}
	return count, sawIsRead
}

// outlookMessageArray finds the messages array in the envelope (top level or a
// known wrapper).
func outlookMessageArray(m map[string]any) []any {
	keys := []string{"value", "messages", "items"}
	for _, k := range keys {
		if a, ok := m[k].([]any); ok {
			return a
		}
	}
	for _, w := range []string{"data", "response_data", "result"} {
		if inner, ok := m[w].(map[string]any); ok {
			for _, k := range keys {
				if a, ok := inner[k].([]any); ok {
					return a
				}
			}
		}
	}
	return nil
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

// extractCount pulls an unread count out of a Composio email-list result,
// covering both providers' shapes (and a possible "data"/"response_data"
// wrapper):
//   - Gmail (users.messages.list): resultSizeEstimate / messages[]
//   - Outlook (Graph /messages): "@odata.count" / value[]
//
// Returns (count, true) when a recognized count key is found, else (0, false)
// so callers can log a parse miss. Defensive by design — the exact envelope is
// confirmed from the logged shape on staging.
func extractCount(text string) (int, bool) {
	var m map[string]any
	if err := json.Unmarshal([]byte(text), &m); err != nil {
		return 0, false
	}
	if n, ok := countFromMap(m); ok {
		return n, true
	}
	// Composio sometimes nests the provider payload one level down.
	for _, key := range []string{"data", "response_data", "result"} {
		if inner, ok := m[key].(map[string]any); ok {
			if n, ok := countFromMap(inner); ok {
				return n, true
			}
		}
	}
	return 0, false
}

func countFromMap(m map[string]any) (int, bool) {
	if v, ok := m["resultSizeEstimate"].(float64); ok {
		return int(v), true
	}
	if v, ok := m["@odata.count"].(float64); ok {
		return int(v), true
	}
	if msgs, ok := m["messages"].([]any); ok {
		return len(msgs), true
	}
	if vals, ok := m["value"].([]any); ok {
		return len(vals), true
	}
	return 0, false
}

// jsonShape returns the top-level keys of a JSON object (plus one nested level
// for known wrappers) for safe diagnostic logging — keys only, never values, so
// no email content is logged.
func jsonShape(text string) string {
	var m map[string]any
	if err := json.Unmarshal([]byte(text), &m); err != nil {
		return "non-object(" + clip(text, 40) + ")"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
		if inner, ok := m[k].(map[string]any); ok && (k == "data" || k == "response_data" || k == "result") {
			for ik := range inner {
				keys = append(keys, k+"."+ik)
			}
		}
	}
	return fmt.Sprintf("%v", keys)
}
