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
// The extension performs fill / click / select / press_enter on an element by CSS
// selector using React-safe native setters + synthetic input/change/keyboard events.
func NewExecuteActionTool() Tool {
	return &clientTool{
		name: "execute_action",
		desc: "Performs a single action on the current page by CSS selector. Call refresh_page_content " +
			"first to see the elements and find the right selector. Actions: " +
			"'fill' (types into an input/textarea — requires value), " +
			"'click' (clicks a button/link/checkbox), " +
			"'select' (picks an option in a <select> — requires value), " +
			"'press_enter' (submits a form by simulating Enter on an input/textarea — preferred " +
			"over click for search forms like Google/GitHub where a visible submit button may be " +
			"hidden or non-functional until interaction).",
		params: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"selector": map[string]any{
					"type":        "string",
					"description": "CSS selector of the target element",
				},
				"action": map[string]any{
					"type":        "string",
					"enum":        []string{"fill", "click", "select", "press_enter"},
					"description": "The action to perform",
				},
				"value": map[string]any{
					"type":        "string",
					"description": "Value for 'fill' or 'select'. Not used for 'click' or 'press_enter'.",
				},
			},
			"required": []string{"selector", "action"},
		},
	}
}

// NewExecuteJSTool returns the execute_js escape-hatch client tool.
// The extension runs arbitrary JavaScript in the page's MAIN world (full
// access to page globals, React/Vue state, etc.) and returns a JSON-
// serialised result. Use when execute_action's structured primitives
// (fill/click/select/press_enter) don't cover the interaction — custom
// comboboxes, shadow DOM, reading arbitrary page state, multi-step
// DOM manipulation.
func NewExecuteJSTool() Tool {
	return &clientTool{
		name: "execute_js",
		desc: "Runs arbitrary JavaScript in the user's current browser tab (MAIN world — " +
			"sees page globals, React/Vue state, shadow roots). The `code` is wrapped in an " +
			"async IIFE, so both expressions and multi-statement blocks work, and you can " +
			"`await` and `return`. The return value is JSON-stringified (DOM nodes / Maps / " +
			"etc. fall back to String). Use this as an ESCAPE HATCH when execute_action " +
			"cannot reach the element — custom comboboxes (not native <select>), shadow DOM, " +
			"reading computed styles, triggering multi-step widget interactions. For simple " +
			"fill/click/select/press_enter prefer execute_action — it's more reliable.",
		params: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"code": map[string]any{
					"type": "string",
					"description": "JavaScript body to execute. Use `return X` to return a value. " +
						"Examples: `return document.title` — read page title. " +
						"`document.querySelector('.btn').click(); return 'ok'` — click a button. " +
						"`return Array.from(document.querySelectorAll('.item')).map(e => e.textContent)` — list items. " +
						"`const el = document.querySelector('[role=combobox]'); el.click(); await new Promise(r => setTimeout(r, 200)); document.querySelector('[role=option][data-value=\"X\"]')?.click(); return 'selected'` — open a combobox and pick an option.",
				},
			},
			"required": []string{"code"},
		},
	}
}

// NewNavigateTool returns the navigate client tool.
// The extension calls chrome.tabs.update to navigate the active tab to the given URL.
func NewNavigateTool() Tool {
	return &clientTool{
		name: "navigate",
		desc: "Navigates the user's active browser tab to a new URL. Use this to open a job listing, " +
			"follow an 'Apply' link that opens a new page, or move to a company's external careers site. " +
			"Navigation starts immediately and returns before the page finishes loading — always follow " +
			"with wait_for_element or refresh_page_content to confirm the new page is ready.",
		params: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"url": map[string]any{
					"type":        "string",
					"description": "Absolute URL to navigate to (must start with https://)",
				},
			},
			"required": []string{"url"},
		},
	}
}

// NewScrollTool returns the scroll client tool.
// The extension scrolls the page so lazy-loaded content and off-screen elements become visible.
func NewScrollTool() Tool {
	return &clientTool{
		name: "scroll",
		desc: "Scrolls the user's active browser tab to reveal more content. Use when the target element " +
			"is not yet in the viewport, the page uses infinite scroll, or content loads lazily on scroll. " +
			"After scrolling call refresh_page_content to capture newly visible elements.",
		params: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"direction": map[string]any{
					"type":        "string",
					"enum":        []string{"down", "up", "top", "bottom"},
					"description": "'down'/'up' scroll by amount px; 'top'/'bottom' jump to page start/end",
				},
				"amount": map[string]any{
					"type":        "integer",
					"description": "Pixels to scroll for 'down'/'up'. Ignored for 'top'/'bottom'. Default 600.",
				},
			},
			"required": []string{"direction"},
		},
	}
}

// NewWaitForElementTool returns the wait_for_element client tool.
// Polls the DOM until a CSS selector matches or the timeout expires.
func NewWaitForElementTool() Tool {
	return &clientTool{
		name: "wait_for_element",
		desc: "Waits for an element to appear in the DOM — use after clicking a button that opens a modal, " +
			"submitting a form that triggers an AJAX response, or navigating to a new page. " +
			"Polls every 200 ms up to timeout_ms (max 10 000). Returns success when found, error on timeout.",
		params: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"selector": map[string]any{
					"type":        "string",
					"description": "CSS selector to wait for",
				},
				"timeout_ms": map[string]any{
					"type":        "integer",
					"description": "Max wait in milliseconds (default 5000, capped at 10000)",
				},
			},
			"required": []string{"selector"},
		},
	}
}

// NewUploadFileTool returns the upload_file client tool.
// The extension reads the file from its local storage by key and assigns it to a file input.
func NewUploadFileTool() Tool {
	return &clientTool{
		name: "upload_file",
		desc: "Attaches a file stored in the extension (e.g. the user's resume) to a file input on the page. " +
			"The user must have saved the file in extension settings first. " +
			"Use file_key 'resume' for CV/resume uploads. " +
			"Dispatches a change event so the page's upload handler fires normally.",
		params: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"selector": map[string]any{
					"type":        "string",
					"description": "CSS selector of the <input type=\"file\"> element",
				},
				"file_key": map[string]any{
					"type":        "string",
					"description": "Key of the stored file to upload, e.g. 'resume' or 'cover_letter'",
				},
			},
			"required": []string{"selector", "file_key"},
		},
	}
}

// RegisterClientTools adds the browser-extension client tools to the registry with
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
	r.RegisterWithMetadata(NewExecuteJSTool(), ToolMetadata{
		Group:        "browser",
		Capabilities: []ToolCapability{CapMutating},
		IsClient:     true,
	})
	r.RegisterWithMetadata(NewNavigateTool(), ToolMetadata{
		Group:        "browser",
		Capabilities: []ToolCapability{CapMutating},
		IsClient:     true,
	})
	r.RegisterWithMetadata(NewScrollTool(), ToolMetadata{
		Group:        "browser",
		Capabilities: []ToolCapability{CapMutating},
		IsClient:     true,
	})
	r.RegisterWithMetadata(NewWaitForElementTool(), ToolMetadata{
		Group:        "browser",
		Capabilities: []ToolCapability{CapReadOnly},
		IsClient:     true,
	})
	r.RegisterWithMetadata(NewUploadFileTool(), ToolMetadata{
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

