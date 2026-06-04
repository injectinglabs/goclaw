package methods

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/gateway"
	"github.com/nextlevelbuilder/goclaw/internal/i18n"
	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/pkg/protocol"
)

// --- agents.delete ---
// Matching TS src/gateway/server-methods/agents.ts:347-398

func (m *AgentsMethods) handleDelete(ctx context.Context, client *gateway.Client, req *protocol.RequestFrame) {
	locale := store.LocaleFromContext(ctx)
	var params struct {
		AgentID     string `json:"agentId"`
		ID          string `json:"id"` // some clients (web UI) send the agent UUID as `id`
		DeleteFiles bool   `json:"deleteFiles"`
	}
	params.DeleteFiles = true // default
	if req.Params != nil {
		json.Unmarshal(req.Params, &params)
	}

	// Accept the agent reference under either `agentId` or `id`; it is resolved
	// below as either the agent_key or the canonical UUID (dual-identity
	// convention — see docs/agent-identity-conventions.md).
	agentRef := params.AgentID
	if agentRef == "" {
		agentRef = params.ID
	}
	if agentRef == "" {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, i18n.T(locale, i18n.MsgRequired, "agentId")))
		return
	}
	if agentRef == "default" {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, i18n.T(locale, i18n.MsgCannotDeleteDefault)))
		return
	}

	var removedBindings int

	if m.agentStore != nil {
		// --- DB-backed: delete from store. Resolve by UUID or agent_key. ---
		ag, err := resolveAgentInfo(ctx, m.agentStore, agentRef)
		if err != nil {
			client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrNotFound, i18n.T(locale, i18n.MsgAgentNotFound, agentRef)))
			return
		}

		// Guard the tenant default even when referenced by UUID (the string
		// check above only catches the literal "default" agent_key).
		if ag.IsDefault {
			client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, i18n.T(locale, i18n.MsgCannotDeleteDefault)))
			return
		}

		if err := m.agentStore.Delete(ctx, ag.ID); err != nil {
			client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInternal, i18n.T(locale, i18n.MsgFailedToDelete, "agent", fmt.Sprintf("%v", err))))
			return
		}

		m.agents.InvalidateAgent(ag.AgentKey)
		m.agents.Remove(ag.AgentKey)

		// Emit agent:deleted event for async cleanup (e.g. orphaned provider removal)
		if m.eventBus != nil {
			m.eventBus.Broadcast(bus.Event{
				Name: bus.TopicAgentDeleted,
				Payload: bus.AgentDeletedPayload{
					AgentKey: ag.AgentKey,
					Provider: ag.Provider,
					TenantID: store.TenantIDFromContext(ctx),
				},
			})
		}

		// Best-effort delete workspace
		if params.DeleteFiles && ag.Workspace != "" {
			os.RemoveAll(ag.Workspace)
		}
	}

	client.SendResponse(protocol.NewOKResponse(req.ID, map[string]any{
		"ok":              true,
		"agentId":         agentRef,
		"removedBindings": removedBindings,
	}))
	emitAudit(m.eventBus, client, "agent.deleted", "agent", agentRef)
}
