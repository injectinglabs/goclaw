package http

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/nextlevelbuilder/goclaw/internal/providerresolve"
	"github.com/nextlevelbuilder/goclaw/internal/providers"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// emailContent is the parsed email used to draft + address a reply.
type emailContent struct {
	From      string `json:"from"`
	Recipient string `json:"recipient"` // address to reply to
	Subject   string `json:"subject"`
	Body      string `json:"body"`
	ThreadID  string `json:"threadId,omitempty"`
	ID        string `json:"id,omitempty"`
}

// handleDraftReply fetches an email and returns an LLM-drafted reply plus the
// fields the UI needs to send it. Body: {provider,id,threadId,instruction?}.
func (h *InboxHandler) handleDraftReply(w http.ResponseWriter, r *http.Request) {
	userID := store.UserIDFromContext(r.Context())
	if userID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "no user"})
		return
	}
	var req struct {
		Provider    string `json:"provider"`
		ID          string `json:"id"`
		ThreadID    string `json:"threadId"`
		Instruction string `json:"instruction"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Provider == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "provider required"})
		return
	}

	email, err := h.fetchEmail(r.Context(), userID, req.Provider, req.ID, req.ThreadID)
	if err != nil {
		slog.Info("inbox.fetch_email_failed", "user", userID, "provider", req.Provider, "err", err.Error())
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "could not fetch email"})
		return
	}

	draft, status, detail := h.draftReply(r.Context(), userID, email, req.Instruction)

	writeJSON(w, http.StatusOK, map[string]any{
		"from":      email.From,
		"recipient": email.Recipient,
		"subject":   email.Subject,
		"body":      email.Body,
		"bodyLen":   len(email.Body), // 0 → email body didn't parse from Composio
		"threadId":  email.ThreadID,
		"id":        email.ID,
		"draft":     draft,
		"status":    status, // "ok" | "no_provider" | "llm_error"
		"detail":    detail, // error text when status != ok (for diagnosis)
	})
}

// handleSendReply sends a reply via the provider's Composio reply action.
// Body: {provider,id,threadId,recipient,body}.
func (h *InboxHandler) handleSendReply(w http.ResponseWriter, r *http.Request) {
	userID := store.UserIDFromContext(r.Context())
	if userID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "no user"})
		return
	}
	var req struct {
		Provider  string `json:"provider"`
		ID        string `json:"id"`
		ThreadID  string `json:"threadId"`
		Recipient string `json:"recipient"`
		Body      string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Body == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body required"})
		return
	}

	var tool string
	var args map[string]any
	switch req.Provider {
	case "gmail":
		tool, args = "GMAIL_REPLY_TO_THREAD", map[string]any{
			"thread_id":       req.ThreadID,
			"recipient_email": req.Recipient,
			"message_body":    req.Body,
		}
	case "outlook":
		tool, args = "OUTLOOK_REPLY_EMAIL", map[string]any{
			"message_id": req.ID,
			"comment":    req.Body, // Graph reply uses "comment"; composio mirrors it
		}
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown provider"})
		return
	}

	if _, err := h.callComposio(r.Context(), userID, tool, args); err != nil {
		slog.Info("inbox.send_reply_failed", "user", userID, "provider", req.Provider, "err", err.Error())
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "send failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// fetchEmail retrieves the email body + sender/subject for drafting a reply.
func (h *InboxHandler) fetchEmail(ctx context.Context, userID, provider, id, threadID string) (emailContent, error) {
	switch provider {
	case "gmail":
		text, err := h.callComposio(ctx, userID, "GMAIL_FETCH_MESSAGE_BY_THREAD_ID", map[string]any{
			"thread_id": threadID,
		})
		if err != nil {
			return emailContent{}, err
		}
		slog.Info("inbox.gmail_fetch_message", "user", userID, "shape", jsonShape(text))
		return parseGmailEmail(text, id, threadID), nil
	case "outlook":
		text, err := h.callComposio(ctx, userID, "OUTLOOK_GET_MESSAGE", map[string]any{
			"message_id": id,
		})
		if err != nil {
			return emailContent{}, err
		}
		slog.Info("inbox.outlook_get_message", "user", userID, "shape", jsonShape(text))
		return parseOutlookEmail(text, id), nil
	}
	return emailContent{}, errUnknownProvider
}

var errUnknownProvider = &inboxError{"unknown provider"}

type inboxError struct{ msg string }

func (e *inboxError) Error() string { return e.msg }

// draftReply asks the tenant's background LLM to write a reply body. Returns ""
// when no provider is wired (the UI then starts from an empty editor).
func (h *InboxHandler) draftReply(ctx context.Context, userID string, email emailContent, instruction string) (string, string, string) {
	if h.registry == nil {
		slog.Info("inbox.draft_no_registry")
		return "", "no_provider", "registry not wired"
	}
	tenantID := store.TenantIDFromContext(ctx)
	// Use the SAME provider+model as the default chat agent ("llm-service" /
	// "default") so drafting works wherever chat works. Fall back to the
	// background-provider resolver only if llm-service isn't registered.
	model := "default"
	provider, err := h.registry.GetForTenant(tenantID, "llm-service")
	if err != nil || provider == nil {
		provider, model = providerresolve.ResolveBackgroundProvider(ctx, tenantID, h.registry, h.sysConfigs)
	}
	if provider == nil {
		slog.Info("inbox.draft_no_provider", "tenant", tenantID.String())
		return "", "no_provider", "no provider resolved"
	}
	sys := "You draft email replies on the user's behalf. Output ONLY the reply body — " +
		"no subject line, no 'Subject:', no quoted original, no placeholder signature. " +
		"Be concise, clear, and professional, matching the tone of the original."
	user := "Reply to this email.\nFrom: " + email.From + "\nSubject: " + email.Subject + "\n\n" + clip(email.Body, 4000)
	if strings.TrimSpace(instruction) != "" {
		user += "\n\nExtra instruction for the reply: " + instruction
	}
	resp, err := provider.Chat(ctx, providers.ChatRequest{
		Messages: []providers.Message{
			{Role: "system", Content: sys},
			{Role: "user", Content: user},
		},
		Model: model,
		Options: map[string]any{
			providers.OptMaxTokens:     800,
			providers.OptTemperature:   0.4,
			providers.OptThinkingLevel: "off",
			// Attribution the llm-service expects (the agent pipeline sets these
			// on every call). Without them a bare Chat can be rejected.
			providers.OptUserID:   userID,
			providers.OptTenantID: tenantID.String(),
		},
	})
	if err != nil {
		slog.Info("inbox.draft_failed", "err", err.Error(), "model", model, "provider", provider.Name())
		return "", "llm_error", clip(err.Error(), 240)
	}
	return strings.TrimSpace(resp.Content), "ok", ""
}

// parseGmailEmail pulls body/from/subject from a GMAIL_FETCH_MESSAGE_BY_THREAD_ID
// result (a thread → messages). Uses the latest message. Defensive across shapes.
func parseGmailEmail(text, id, threadID string) emailContent {
	out := emailContent{ID: id, ThreadID: threadID}
	var m map[string]any
	if json.Unmarshal([]byte(text), &m) != nil {
		return out
	}
	arr := messageArray(m)
	var msg map[string]any
	if len(arr) > 0 {
		// latest message in the thread
		if mm, ok := arr[len(arr)-1].(map[string]any); ok {
			msg = mm
		}
	} else {
		msg = m // single-message shape
	}
	if msg == nil {
		return out
	}
	out.From = strField(msg, "sender", "from", "From")
	out.Subject = strField(msg, "subject", "Subject")
	out.Body = strField(msg, "messageText", "body", "snippet", "preview", "text")
	if out.From == "" || out.Subject == "" {
		hf, hs := gmailHeaders(msg)
		if out.From == "" {
			out.From = hf
		}
		if out.Subject == "" {
			out.Subject = hs
		}
	}
	out.Recipient = out.From // reply goes back to the sender
	return out
}

// parseOutlookEmail pulls body/from/subject from an OUTLOOK_GET_MESSAGE result.
func parseOutlookEmail(text, id string) emailContent {
	out := emailContent{ID: id}
	var m map[string]any
	if json.Unmarshal([]byte(text), &m) != nil {
		return out
	}
	msg := m
	// Composio may nest under data/response_data/result.
	for _, w := range []string{"data", "response_data", "result", "message"} {
		if inner, ok := m[w].(map[string]any); ok {
			msg = inner
			break
		}
	}
	out.From = outlookFrom(msg)
	out.Recipient = out.From
	out.Subject = strField(msg, "subject", "Subject")
	if bodyObj, ok := msg["body"].(map[string]any); ok { // Graph: body.content
		out.Body = strField(bodyObj, "content")
	}
	if out.Body == "" {
		out.Body = strField(msg, "bodyPreview", "body_preview", "preview")
	}
	return out
}
