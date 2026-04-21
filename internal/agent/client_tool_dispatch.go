package agent

import (
	"context"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/providers"
	"github.com/nextlevelbuilder/goclaw/internal/tools"
	"github.com/nextlevelbuilder/goclaw/pkg/protocol"
)

// ClientToolTimeout is how long the agent loop will wait for a client-executed
// tool (refresh_page_content, execute_action) to respond before returning a
// timeout error to the LLM. The client posts results over the same WS that
// received the client_tool_call event.
const ClientToolTimeout = 10 * time.Second

// dispatchClientTool emits a client_tool_call event to the WS client that owns
// this run, then blocks on a registry-managed result channel until the matching
// tool_result arrives, ctx is cancelled, or ClientToolTimeout elapses.
//
// The emitRun callback is the same one used for tool.call / tool.result events,
// so the event rides the existing agent bus → per-user filter → SendEvent path.
// The extension recognises payload.type == "client_tool_call" and routes to its
// content-script bridge.
func (l *Loop) dispatchClientTool(
	ctx context.Context,
	req *RunRequest,
	emitRun func(AgentEvent),
	tc providers.ToolCall,
) *tools.Result {
	if l.registry == nil {
		return tools.ErrorResult("client tool: registry unavailable")
	}

	resultCh := l.registry.RegisterClientToolResultCh(tc.ID)
	defer l.registry.UnregisterClientToolResultCh(tc.ID)

	emitRun(AgentEvent{
		Type:    protocol.AgentEventClientToolCall,
		AgentID: l.id,
		RunID:   req.RunID,
		Payload: map[string]any{
			"id":    tc.ID,
			"name":  tc.Name,
			"input": tc.Arguments,
		},
	})

	select {
	case res := <-resultCh:
		if res == nil {
			return tools.ErrorResult("client tool: empty result from extension")
		}
		return res
	case <-ctx.Done():
		return tools.ErrorResult("client tool: run cancelled before client responded")
	case <-time.After(ClientToolTimeout):
		return tools.ErrorResult("client tool: execution timed out after 10s")
	}
}
