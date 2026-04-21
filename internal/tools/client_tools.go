package tools

import "context"

// Client tools are executed in the connected client (browser extension), not on the server.
// The registry keeps their schema so the LLM sees them in its tool list, but Execute is
// a safety stub — the agent loop intercepts via ToolMetadata.IsClient and dispatches the
// call to the WS client instead.

// clientTool is the server-side placeholder for a client-executed tool. Its Execute never
// runs in the normal path; the dispatcher in the agent loop checks IsClient metadata first.
type clientTool struct {
	name   string
	desc   string
	params map[string]any
}

func (t *clientTool) Name() string                 { return t.name }
func (t *clientTool) Description() string          { return t.desc }
func (t *clientTool) Parameters() map[string]any   { return t.params }
func (t *clientTool) Execute(_ context.Context, _ map[string]any) *Result {
	return ErrorResult("client tool must be dispatched to the extension, not executed server-side")
}

// ─── Tool definitions ─────────────────────────────────────────────────────────────

// NewRefreshPageContentTool returns the refresh_page_content client tool.
// The extension returns a compact semantic DOM snapshot of the user's active tab.
func NewRefreshPageContentTool() Tool {
	return &clientTool{
		name: "refresh_page_content",
		desc: "Captures a compact semantic snapshot of the user's current browser page — " +
			"URL, title, interactive elements (inputs, buttons, links) with stable CSS selectors, " +
			"headings, and a visible-text preview. Call this whenever you need to understand what's " +
			"on the page or find element selectors. Call again after actions that likely changed " +
			"the page (form submit, navigation, AJAX updates). You have the page URL and title via " +
			"the page_hint in the user's message — use that to decide whether the current task " +
			"actually needs the page snapshot.",
		params: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
			"required":   []string{},
		},
	}
}

// NewExecuteActionTool returns the execute_action client tool.
// The extension performs fill / click / select on an element by CSS selector using
// React-safe native setters + synthetic input/change events.
func NewExecuteActionTool() Tool {
	return &clientTool{
		name: "execute_action",
		desc: "Performs a single action on the current page by CSS selector. Call refresh_page_content " +
			"first to see the elements and find the right selector. Actions: 'fill' (types into an " +
			"input/textarea — requires value), 'click' (clicks a button/link/checkbox), 'select' " +
			"(picks an option in a <select> — requires value).",
		params: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"selector": map[string]any{
					"type":        "string",
					"description": "CSS selector of the target element",
				},
				"action": map[string]any{
					"type":        "string",
					"enum":        []string{"fill", "click", "select"},
					"description": "The action to perform",
				},
				"value": map[string]any{
					"type":        "string",
					"description": "Value for 'fill' or 'select'. Not used for 'click'.",
				},
			},
			"required": []string{"selector", "action"},
		},
	}
}

// RegisterClientTools adds refresh_page_content and execute_action to the registry with
// IsClient=true. Safe to call multiple times (overwrites prior registration).
func RegisterClientTools(r *Registry) {
	r.RegisterWithMetadata(NewRefreshPageContentTool(), ToolMetadata{
		Group:        "browser",
		Capabilities: []ToolCapability{CapReadOnly},
		IsClient:     true,
	})
	r.RegisterWithMetadata(NewExecuteActionTool(), ToolMetadata{
		Group:        "browser",
		Capabilities: []ToolCapability{CapMutating},
		IsClient:     true,
	})
}

// ─── Result channel routing ────────────────────────────────────────────────────────
//
// When the agent loop invokes a client tool, it registers a buffered channel keyed by
// the tool_call_id, emits an event to the WS client, and blocks on the channel. When
// the client posts a tool_result, the WS handler routes the Result into that channel
// and the loop continues. The registry owns the map since it is the single shared
// instance reachable from both the loop (via l.registry) and the WS handler (via the
// ChatMethods deps).

// RegisterClientToolResultCh creates a buffered (1) channel for the given tool_call_id
// and stores it. Callers must UnregisterClientToolResultCh when done to avoid leaks.
// Buffer size 1 lets the WS handler drop results if the waiting goroutine already gave up.
func (r *Registry) RegisterClientToolResultCh(toolCallID string) chan *Result {
	ch := make(chan *Result, 1)
	r.clientToolChannels.Store(toolCallID, ch)
	return ch
}

// UnregisterClientToolResultCh drops the channel entry. Idempotent.
func (r *Registry) UnregisterClientToolResultCh(toolCallID string) {
	r.clientToolChannels.Delete(toolCallID)
}

// RouteClientToolResult delivers a result to the waiting goroutine identified by
// toolCallID. Returns true if the channel was found and the send succeeded, false
// if there was no pending call or the receiver had already timed out.
func (r *Registry) RouteClientToolResult(toolCallID string, result *Result) bool {
	v, ok := r.clientToolChannels.LoadAndDelete(toolCallID)
	if !ok {
		return false
	}
	ch, ok := v.(chan *Result)
	if !ok {
		return false
	}
	select {
	case ch <- result:
		return true
	default:
		// Receiver already gave up (timeout / ctx cancel). Drop silently.
		return false
	}
}

