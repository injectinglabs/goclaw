package agent

import (
	"context"
	"slices"

	"github.com/nextlevelbuilder/goclaw/internal/providers"
	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/internal/tools"
)

// buildFilteredTools resolves the per-iteration tool definitions based on policy,
// disabled tools, bootstrap mode, skill visibility, channel type, and iteration budget.
// Per-user MCP tools must be registered in the Registry before calling this function
// (via getUserMCPTools) so they are included in policy filtering and execution.
// Returns tool definitions for the provider, an allowed-tools map for execution validation,
// and the (potentially modified) messages slice when final-iteration stripping appends a hint.
func (l *Loop) buildFilteredTools(ctx context.Context, req *RunRequest, hadBootstrap bool, iteration, maxIter int, messages []providers.Message) ([]providers.ToolDefinition, map[string]bool, []providers.Message) {
	// Build provider request with policy-filtered tools.
	var toolDefs []providers.ToolDefinition
	var allowedTools map[string]bool
	if l.toolPolicy != nil {
		toolDefs = l.toolPolicy.FilterTools(l.tools, l.id, l.provider.Name(), l.agentToolPolicy, req.ToolAllow, false, false)
		allowedTools = make(map[string]bool, len(toolDefs))
		for _, td := range toolDefs {
			allowedTools[td.Function.Name] = true
		}
	} else {
		toolDefs = l.tools.ProviderDefs()
	}

	// V3 orchestration mode filtering: hide tools the agent shouldn't see.
	// spawn: no delegate/team_tasks. delegate: no team_tasks. team: all.
	if orchDeny := orchModeDenyTools(l.orchMode); len(orchDeny) > 0 {
		filtered := toolDefs[:0:0]
		for _, td := range toolDefs {
			if !orchDeny[td.Function.Name] {
				filtered = append(filtered, td)
			} else {
				delete(allowedTools, td.Function.Name)
			}
		}
		toolDefs = filtered
	}

	// Per-tenant tool exclusions: remove tools disabled for this agent's tenant.
	if len(l.disabledTools) > 0 {
		filtered := toolDefs[:0]
		for _, td := range toolDefs {
			if !l.disabledTools[td.Function.Name] {
				filtered = append(filtered, td)
			} else {
				delete(allowedTools, td.Function.Name)
			}
		}
		toolDefs = filtered
	}

	// Skill tool lock: when a skill that needs a forced deterministic tool path
	// has been activated this run (e.g. parallel-research-sheet), strip its
	// denied tools so the model can't fall back to the fabrication escape hatch
	// (exec/write_file). Leaves the skill's intended tool (research_sheet) as the
	// only path to the deliverable. Scoped per-run via the ctx-injected lock.
	if lock := tools.SkillToolLockFromCtx(ctx); lock != nil {
		var denied map[string]bool
		for _, slug := range tools.SkillToolDenylistSlugs() {
			if !lock.IsActive(slug) {
				continue
			}
			for name := range tools.SkillDeniedTools(slug) {
				if denied == nil {
					denied = make(map[string]bool)
				}
				denied[name] = true
			}
		}
		if len(denied) > 0 {
			filtered := toolDefs[:0:0]
			for _, td := range toolDefs {
				if denied[td.Function.Name] {
					delete(allowedTools, td.Function.Name)
					continue
				}
				filtered = append(filtered, td)
			}
			toolDefs = filtered
		}
	}

	// Bootstrap mode: restrict API tool definitions to write_file only (open agents).
	// Predefined agents keep all tools — BOOTSTRAP.md guides behavior.
	if hadBootstrap && l.agentType != store.AgentTypePredefined {
		var bootstrapDefs []providers.ToolDefinition
		for _, td := range toolDefs {
			if bootstrapToolAllowlist[td.Function.Name] {
				bootstrapDefs = append(bootstrapDefs, td)
			}
		}
		toolDefs = bootstrapDefs
	}

	// Hide skill_manage from LLM when skill_evolve is off.
	// Tool stays in the registry (shared) but won't appear in API tool definitions.
	if !l.skillEvolve {
		filtered := toolDefs[:0:0]
		for _, td := range toolDefs {
			if td.Function.Name != "skill_manage" {
				filtered = append(filtered, td)
			}
		}
		toolDefs = filtered
	}

	// Strip browser-extension client tools when the caller cannot service them.
	// Client tools (execute_action, execute_js, refresh_page_content, navigate, …)
	// are registered once in the global registry and would otherwise leak into
	// every WS palette — including the website chat, which has no tabs/DOM and
	// would only return stub errors, burning the model's tool-call budget on
	// dead-end calls. ClientKind="extension" keeps them; "website" (or any
	// other non-empty non-"extension" value) strips them. Empty preserves
	// pre-flag behavior so non-WS channels and pre-rollout extension builds
	// keep working unchanged.
	if req.ClientKind != "" && req.ClientKind != "extension" {
		filtered := toolDefs[:0:0]
		for _, td := range toolDefs {
			if l.registry.GetMetadata(td.Function.Name).IsClient {
				delete(allowedTools, td.Function.Name)
				continue
			}
			filtered = append(filtered, td)
		}
		toolDefs = filtered
	}

	// Hide channel-specific tools when channel type doesn't match.
	if req.ChannelType != "" {
		filtered := toolDefs[:0:0]
		for _, td := range toolDefs {
			if tool, ok := l.tools.Get(td.Function.Name); ok {
				if ca, ok := tool.(tools.ChannelAware); ok {
					if !slices.Contains(ca.RequiredChannelTypes(), req.ChannelType) {
						continue
					}
				}
			}
			filtered = append(filtered, td)
		}
		toolDefs = filtered
	}

	// Final iteration: strip all tools to force a text-only response.
	// Without this the model may keep requesting tools and exit with "...".
	if iteration == maxIter {
		toolDefs = nil
		messages = append(messages, providers.Message{
			Role:    "user",
			Content: "[System] Final iteration reached. Summarize all findings and respond to the user now. No more tool calls allowed.",
		})
	}

	return toolDefs, allowedTools, messages
}
