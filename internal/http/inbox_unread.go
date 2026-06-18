package http

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/providers"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// InboxHandler serves unread-count lookups for the browser extension's
// background badge (v0: interval polling). It asks composio-mcp for the
// acting user's unread email count per provider. Real-time push (Composio
// triggers → webhook) is a later v1; this is a cheap on-demand poll.
type InboxHandler struct {
	composioURL string // base URL of composio-mcp, e.g. http://composio-mcp:9300
	httpClient  *http.Client
	registry    *providers.Registry     // for the reply-draft LLM call (optional)
	sysConfigs  store.SystemConfigStore // tenant background-provider resolution (optional)

	// Push half (Composio triggers → composio-mcp socket → internal forward →
	// WS), wired via EnablePush.
	pushEnabled   bool               // gate provisioning + internal endpoint
	pub           bus.EventPublisher // broadcasts inbox.updated
	tenants       store.TenantStore  // user→tenant for event scoping
	internalToken string             // optional shared token for the internal forward
	provisioned   sync.Map           // dedup trigger enable (key: user|toolkit|acct)

	// pushSender, when set, delivers a Web Push notification to the user's
	// subscribed browsers on each new-mail event (best-effort). Wired via
	// SetPushSender.
	pushSender func(ctx context.Context, userID string, payload []byte)
}

// SetPushSender wires a Web Push delivery callback invoked from pushInboxUpdated
// on each new-mail event. Best-effort; nil disables Web Push delivery.
func (h *InboxHandler) SetPushSender(fn func(ctx context.Context, userID string, payload []byte)) {
	h.pushSender = fn
}

// NewInboxHandler constructs the inbox handler. registry+sysConfigs power the
// reply-draft LLM call; pass nil to disable drafting (count/list/mark-read still work).
func NewInboxHandler(composioURL string, registry *providers.Registry, sysConfigs store.SystemConfigStore) *InboxHandler {
	return &InboxHandler{
		composioURL: composioURL,
		httpClient:  &http.Client{Timeout: 20 * time.Second},
		registry:    registry,
		sysConfigs:  sysConfigs,
	}
}

// RegisterRoutes registers the inbox routes on the given mux.
func (h *InboxHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/inbox/unread", requireAuth("", h.handleUnread))
	mux.HandleFunc("POST /v1/inbox/mark-read", requireAuth("", h.handleMarkRead))
	mux.HandleFunc("POST /v1/inbox/delete", requireAuth("", h.handleDelete))
	mux.HandleFunc("POST /v1/inbox/draft-reply", requireAuth("", h.handleDraftReply))
	mux.HandleFunc("POST /v1/inbox/send-reply", requireAuth("", h.handleSendReply))
	// Internal forward from composio-mcp's trigger subscription — token-guarded,
	// internal network only (no user-auth middleware).
	mux.HandleFunc("POST /v1/internal/inbox-event", h.handleInternalInboxEvent)
	// Public, non-sensitive backbone status for confirming the push pipeline
	// (no auth, no user data) — reports whether inbox push is enabled and proxies
	// composio-mcp's subscription health.
	mux.HandleFunc("GET /v1/push/health", h.handlePushHealth)
}

// handlePushHealth reports the email-push backbone status without auth or user
// data: whether goclaw has push enabled, plus composio-mcp's subscription state
// (is the triggers.subscribe socket up, how many active triggers, events seen).
func (h *InboxHandler) handlePushHealth(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"inbox_push_enabled": h.pushEnabled}
	req, err := http.NewRequestWithContext(r.Context(), "GET", h.composioURL+"/triggers/health", nil)
	if err == nil {
		resp, derr := h.httpClient.Do(req)
		if derr == nil {
			defer resp.Body.Close()
			var c map[string]any
			if json.NewDecoder(resp.Body).Decode(&c) == nil {
				out["composio"] = c
			} else {
				out["composio_error"] = "decode failed"
			}
		} else {
			out["composio_error"] = derr.Error()
		}
	}
	writeJSON(w, http.StatusOK, out)
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
		Provider  string `json:"provider"`
		ID        string `json:"id"`
		AccountID string `json:"accountId"`
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

	if _, err := h.callComposio(r.Context(), userID, body.AccountID, tool, args); err != nil {
		slog.Info("inbox.mark_read_failed", "user", userID, "provider", body.Provider, "err", err.Error())
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "mark-read failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleDelete removes one message from the user's mailbox: Gmail → move to
// trash (reversible), Outlook → delete (moves to Deleted Items).
// Body: {"provider":"gmail"|"outlook","id":"<message id>","accountId":"<acct>"}.
func (h *InboxHandler) handleDelete(w http.ResponseWriter, r *http.Request) {
	userID := store.UserIDFromContext(r.Context())
	if userID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "no user"})
		return
	}
	var body struct {
		Provider  string `json:"provider"`
		ID        string `json:"id"`
		AccountID string `json:"accountId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "provider and id required"})
		return
	}

	var tool string
	switch body.Provider {
	case "gmail":
		tool = "GMAIL_MOVE_TO_TRASH"
	case "outlook":
		tool = "OUTLOOK_DELETE_MESSAGE"
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown provider"})
		return
	}

	if _, err := h.callComposio(r.Context(), userID, body.AccountID, tool, map[string]any{"message_id": body.ID}); err != nil {
		slog.Info("inbox.delete_failed", "user", userID, "provider", body.Provider, "err", err.Error())
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "delete failed"})
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

	// Lazily ensure Composio email triggers exist for this user's mailboxes so
	// future new mail pushes over WS. Deduped + async — doesn't slow the poll.
	if h.pushEnabled {
		go h.ProvisionTriggers(context.Background(), userID)
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
	Provider  string `json:"provider"`
	Account   string `json:"account,omitempty"`   // which mailbox (email/alias) for multi-account
	AccountID string `json:"accountId,omitempty"` // connectedAccountId — for mark-read/reply targeting
	ID        string `json:"id"`                  // message id — used to mark-read / reply
	ThreadID  string `json:"threadId,omitempty"`  // gmail thread id — used to reply
	From      string `json:"from"`
	Subject   string `json:"subject"`
	Date      string `json:"date,omitempty"`
}

// inboxAccount is one connected mailbox returned by composio-mcp /accounts.
type inboxAccount struct {
	ID    string `json:"id"`
	Alias string `json:"alias"`
}

// listAccounts returns the user's connected accounts for a toolkit via the
// composio-mcp /accounts endpoint. Returns nil on error/none.
func (h *InboxHandler) listAccounts(ctx context.Context, userID, toolkit string) []inboxAccount {
	body, _ := json.Marshal(map[string]any{"toolkit": toolkit})
	req, err := http.NewRequestWithContext(ctx, "POST", h.composioURL+"/accounts", bytes.NewReader(body))
	if err != nil {
		return nil
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Proxy-User", userID)
	resp, err := h.httpClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil
	}
	var out struct {
		Accounts []inboxAccount `json:"accounts"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil {
		return nil
	}
	return out.Accounts
}

// accountLabel resolves a human label for an account: its alias, else the
// mailbox email via *_GET_PROFILE (best-effort), else "".
func (h *InboxHandler) accountLabel(ctx context.Context, userID, toolkit string, a inboxAccount) string {
	if a.Alias != "" {
		return a.Alias
	}
	if a.ID == "" {
		return ""
	}
	var tool string
	switch toolkit {
	case "gmail":
		tool = "GMAIL_GET_PROFILE"
	case "outlook":
		tool = "OUTLOOK_GET_PROFILE"
	default:
		return ""
	}
	text, err := h.callComposio(ctx, userID, a.ID, tool, map[string]any{})
	if err != nil {
		return ""
	}
	var m map[string]any
	if json.Unmarshal([]byte(text), &m) != nil {
		return ""
	}
	if e := strField(m, "emailAddress", "email", "mail", "userPrincipalName"); e != "" {
		return e
	}
	for _, w := range []string{"data", "response_data", "result"} {
		if inner, ok := m[w].(map[string]any); ok {
			if e := strField(inner, "emailAddress", "email", "mail", "userPrincipalName"); e != "" {
				return e
			}
		}
	}
	return ""
}

// gmailUnread returns the user's unread INBOX count + message previews via
// composio GMAIL_FETCH_EMAILS. Returns (0, nil) on any error.
func (h *InboxHandler) gmailUnread(ctx context.Context, userID string) (int, []unreadMessage) {
	total := 0
	var all []unreadMessage
	for _, a := range h.accountsOrDefault(ctx, userID, "gmail") {
		text, err := h.callComposio(ctx, userID, a.ID, "GMAIL_FETCH_EMAILS", map[string]any{
			"query":       "is:unread in:inbox",
			"max_results": 25,
		})
		if err != nil {
			slog.Info("inbox.gmail_unread_failed", "user", userID, "acct", a.ID, "err", err.Error())
			continue
		}
		msgs := extractGmailMessages(text)
		label := h.accountLabel(ctx, userID, "gmail", a)
		for i := range msgs {
			msgs[i].Account = label
			msgs[i].AccountID = a.ID
		}
		n, ok := extractCount(text)
		if !ok {
			n = len(msgs)
		}
		total += n
		all = append(all, msgs...)
		slog.Info("inbox.gmail_unread", "user", userID, "acct", a.ID, "count", n, "shape", jsonShape(text))
	}
	return total, all
}

// accountsOrDefault returns the user's connected accounts for a toolkit, or a
// single empty-id account (= the user's default connection) when none are
// listed — preserving single-account behavior.
func (h *InboxHandler) accountsOrDefault(ctx context.Context, userID, toolkit string) []inboxAccount {
	accts := h.listAccounts(ctx, userID, toolkit)
	if len(accts) == 0 {
		return []inboxAccount{{}}
	}
	return accts
}

// outlookUnread returns the user's unread inbox count + message previews. Counts
// unread client-side via each message's isRead field (Graph always returns it),
// rather than relying on an unreliable server-side filter arg.
func (h *InboxHandler) outlookUnread(ctx context.Context, userID string) (int, []unreadMessage) {
	total := 0
	var all []unreadMessage
	for _, a := range h.accountsOrDefault(ctx, userID, "outlook") {
		text, err := h.callComposio(ctx, userID, a.ID, "OUTLOOK_LIST_MESSAGES", map[string]any{
			"folder": "inbox",
			"top":    50,
		})
		if err != nil {
			slog.Info("inbox.outlook_unread_failed", "user", userID, "acct", a.ID, "err", err.Error())
			continue
		}
		msgs := extractOutlookMessages(text)
		label := h.accountLabel(ctx, userID, "outlook", a)
		for i := range msgs {
			msgs[i].Account = label
			msgs[i].AccountID = a.ID
		}
		total += len(msgs)
		all = append(all, msgs...)
		slog.Info("inbox.outlook_unread", "user", userID, "acct", a.ID, "count", len(msgs), "shape", jsonShape(text))
	}
	return total, all
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

// callComposio invokes a composio-mcp tool. connectedAccountID (optional) targets
// a specific connected account via X-Connected-Account-Id; empty = the user's
// default connection (single-account behavior).
func (h *InboxHandler) callComposio(ctx context.Context, userID, connectedAccountID, tool string, args map[string]any) (string, error) {
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
	if connectedAccountID != "" {
		req.Header.Set("X-Connected-Account-Id", connectedAccountID)
	}

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
