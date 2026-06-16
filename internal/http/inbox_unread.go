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
	mux.HandleFunc("POST /v1/inbox/mark-read", requireAuth("", h.handleMarkRead))
}

// handleMarkRead marks one message read in the user's mailbox.
// Body: {"provider":"gmail"|"outlook","id":"<message id>"}.
func (h *InboxHandler) handleMarkRead(w http.ResponseWriter, r *http.Request) {
	userID := store.UserIDFromContext(r.Context())
	if userID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "no user"})
		return
	}
	var body struct {
		Provider string `json:"provider"`
		ID       string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "provider and id required"})
		return
	}

	var tool string
	var args map[string]any
	switch body.Provider {
	case "gmail":
		// Removing the UNREAD label marks the message read.
		tool, args = "GMAIL_ADD_LABEL_TO_EMAIL", map[string]any{
			"message_id":       body.ID,
			"remove_label_ids": []string{"UNREAD"},
		}
	case "outlook":
		tool, args = "OUTLOOK_UPDATE_MESSAGE", map[string]any{
			"message_id": body.ID,
			"is_read":    true,
		}
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown provider"})
		return
	}

	if _, err := h.callComposio(r.Context(), userID, tool, args); err != nil {
		slog.Info("inbox.mark_read_failed", "user", userID, "provider", body.Provider, "err", err.Error())
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "mark-read failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
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

	gmailN, gmailMsgs := h.gmailUnread(r.Context(), userID)
	outlookN, outlookMsgs := h.outlookUnread(r.Context(), userID)

	messages := append(gmailMsgs, outlookMsgs...)
	if messages == nil {
		messages = []unreadMessage{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"gmail":    gmailN,
		"outlook":  outlookN,
		"total":    gmailN + outlookN,
		"messages": messages, // sender/subject/date for the reminders UI list
	})
}

// unreadMessage is a compact description of one unread email for the UI list.
type unreadMessage struct {
	Provider string `json:"provider"`
	ID       string `json:"id"`                  // message id — used to mark-read / reply
	ThreadID string `json:"threadId,omitempty"`  // gmail thread id — used to reply
	From     string `json:"from"`
	Subject  string `json:"subject"`
	Date     string `json:"date,omitempty"`
}

// gmailUnread returns the user's unread INBOX count + message previews via
// composio GMAIL_FETCH_EMAILS. Returns (0, nil) on any error.
func (h *InboxHandler) gmailUnread(ctx context.Context, userID string) (int, []unreadMessage) {
	text, err := h.callComposio(ctx, userID, "GMAIL_FETCH_EMAILS", map[string]any{
		"query":       "is:unread in:inbox",
		"max_results": 25,
	})
	if err != nil {
		slog.Info("inbox.gmail_unread_failed", "user", userID, "err", err.Error())
		return 0, nil
	}
	msgs := extractGmailMessages(text)
	n, ok := extractCount(text)
	if !ok {
		n = len(msgs)
	}
	// Log the envelope SHAPE (top-level keys only, never email content).
	slog.Info("inbox.gmail_unread", "user", userID, "count", n, "msgs", len(msgs), "shape", jsonShape(text))
	return n, msgs
}

// outlookUnread returns the user's unread inbox count + message previews. Counts
// unread client-side via each message's isRead field (Graph always returns it),
// rather than relying on an unreliable server-side filter arg.
func (h *InboxHandler) outlookUnread(ctx context.Context, userID string) (int, []unreadMessage) {
	text, err := h.callComposio(ctx, userID, "OUTLOOK_LIST_MESSAGES", map[string]any{
		"folder": "inbox",
		"top":    50,
	})
	if err != nil {
		slog.Info("inbox.outlook_unread_failed", "user", userID, "err", err.Error())
		return 0, nil
	}
	msgs := extractOutlookMessages(text)
	slog.Info("inbox.outlook_unread", "user", userID, "count", len(msgs), "shape", jsonShape(text))
	return len(msgs), msgs
}

// countUnreadOutlook counts messages with isRead==false in an
// OUTLOOK_LIST_MESSAGES result. Returns (count, sawIsRead); sawIsRead is false
// when no item exposed an isRead field.
func extractOutlookMessages(text string) []unreadMessage {
	var m map[string]any
	if err := json.Unmarshal([]byte(text), &m); err != nil {
		return nil
	}
	var out []unreadMessage
	for _, it := range messageArray(m) {
		msg, ok := it.(map[string]any)
		if !ok {
			continue
		}
		// Only include items known to be unread (Graph always returns isRead).
		isRead, hasKey := boolField(msg, "isRead", "is_read")
		if !hasKey || isRead {
			continue
		}
		out = append(out, unreadMessage{
			Provider: "outlook",
			ID:       strField(msg, "id", "messageId", "message_id"),
			From:     outlookFrom(msg),
			Subject:  strField(msg, "subject", "Subject"),
			Date:     strField(msg, "receivedDateTime", "received_date_time", "sentDateTime"),
		})
	}
	return out
}

// extractGmailMessages parses message previews from a GMAIL_FETCH_EMAILS result.
// The query already filtered to unread, so every returned message counts. Field
// names vary by Composio version, so it's tolerant (flat sender/subject, or raw
// Gmail payload.headers).
func extractGmailMessages(text string) []unreadMessage {
	var m map[string]any
	if err := json.Unmarshal([]byte(text), &m); err != nil {
		return nil
	}
	var out []unreadMessage
	for _, it := range messageArray(m) {
		msg, ok := it.(map[string]any)
		if !ok {
			continue
		}
		from := strField(msg, "sender", "from", "From")
		subject := strField(msg, "subject", "Subject")
		if from == "" || subject == "" {
			hf, hs := gmailHeaders(msg)
			if from == "" {
				from = hf
			}
			if subject == "" {
				subject = hs
			}
		}
		out = append(out, unreadMessage{
			Provider: "gmail",
			ID:       strField(msg, "messageId", "id", "message_id"),
			ThreadID: strField(msg, "threadId", "thread_id"),
			From:     from,
			Subject:  subject,
			Date:     strField(msg, "messageTimestamp", "date", "internalDate"),
		})
	}
	return out
}

// gmailHeaders pulls From/Subject out of a raw Gmail payload.headers array.
func gmailHeaders(msg map[string]any) (from, subject string) {
	payload, ok := msg["payload"].(map[string]any)
	if !ok {
		return "", ""
	}
	headers, ok := payload["headers"].([]any)
	if !ok {
		return "", ""
	}
	for _, h := range headers {
		hm, ok := h.(map[string]any)
		if !ok {
			continue
		}
		switch strField(hm, "name") {
		case "From":
			from = strField(hm, "value")
		case "Subject":
			subject = strField(hm, "value")
		}
	}
	return from, subject
}

// outlookFrom extracts a display sender from a Graph message (from.emailAddress).
func outlookFrom(msg map[string]any) string {
	if from, ok := msg["from"].(map[string]any); ok {
		if ea, ok := from["emailAddress"].(map[string]any); ok {
			if n := strField(ea, "name"); n != "" {
				return n
			}
			if a := strField(ea, "address"); a != "" {
				return a
			}
		}
	}
	return strField(msg, "sender", "fromAddress")
}

// messageArray finds the messages array in a Composio envelope (top level or a
// known wrapper).
func messageArray(m map[string]any) []any {
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

// strField returns the first non-empty string value among the given keys.
func strField(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// boolField returns the first bool value among the given keys, and whether one was found.
func boolField(m map[string]any, keys ...string) (bool, bool) {
	for _, k := range keys {
		if b, ok := m[k].(bool); ok {
			return b, true
		}
	}
	return false, false
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
