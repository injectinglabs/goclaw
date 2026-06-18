package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/audio"
	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/gateway/methods"
	httpapi "github.com/nextlevelbuilder/goclaw/internal/http"
	mcpbridge "github.com/nextlevelbuilder/goclaw/internal/mcp"
	"github.com/nextlevelbuilder/goclaw/internal/media"
	"github.com/nextlevelbuilder/goclaw/internal/providerresolve"
	"github.com/nextlevelbuilder/goclaw/internal/providers"
	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/internal/store/pg"
	"github.com/nextlevelbuilder/goclaw/internal/tools"
	"github.com/nextlevelbuilder/goclaw/internal/workflow/runtime"
)

// httpHandlers bundles the results of wireHTTP() for passing to wireHTTPHandlersOnServer.
type httpHandlers struct {
	agents           *httpapi.AgentsHandler
	skills           *httpapi.SkillsHandler
	traces           *httpapi.TracesHandler
	mcp              *httpapi.MCPHandler
	channelInstances *httpapi.ChannelInstancesHandler
	providers        *httpapi.ProvidersHandler
	builtinTools     *httpapi.BuiltinToolsHandler
	pendingMessages  *httpapi.PendingMessagesHandler
	teamEvents       *httpapi.TeamEventsHandler
	secureCLI        *httpapi.SecureCLIHandler
	secureCLIGrant   *httpapi.SecureCLIGrantHandler
	mcpUserCreds     *httpapi.MCPUserCredentialsHandler
}

// wireHTTPHandlersOnServer registers all HTTP handler objects onto the gateway server.
// Called after wireHTTP() and wireExtras() have returned their results.
func (d *gatewayDeps) wireHTTPHandlersOnServer(
	h httpHandlers,
	wakeH *httpapi.WakeHandler,
	mcpPool *mcpbridge.Pool,
	postTurn tools.PostTurnProcessor,
	mediaStore *media.Store,
) {
	if h.providers != nil {
		h.providers.SetAPIBaseFallback(d.cfg.Providers.APIBaseForType)
	}
	if h.agents != nil {
		d.server.SetAgentsHandler(h.agents)
	}
	if h.skills != nil {
		d.server.SetSkillsHandler(h.skills)
	}
	if h.traces != nil {
		d.server.SetTracesHandler(h.traces)
	}
	// External wake/trigger API — wakeH was created by caller before invoking this method.
	d.server.SetWakeHandler(wakeH)
	if h.mcp != nil {
		if mcpPool != nil {
			h.mcp.SetPoolEvictor(mcpPool)
		}
		d.server.SetMCPHandler(h.mcp)
	}
	if h.mcpUserCreds != nil {
		d.server.SetMCPUserCredentialsHandler(h.mcpUserCreds)
	}
	if h.channelInstances != nil {
		d.server.SetChannelInstancesHandler(h.channelInstances)
	}
	if h.providers != nil {
		d.server.SetProvidersHandler(h.providers)
	}
	if h.teamEvents != nil {
		d.server.SetTeamEventsHandler(h.teamEvents)
	}
	if d.pgStores != nil && d.pgStores.Teams != nil {
		d.server.SetTeamAttachmentsHandler(httpapi.NewTeamAttachmentsHandler(d.pgStores.Teams, d.workspace))
		d.server.SetWorkspaceUploadHandler(httpapi.NewWorkspaceUploadHandler(d.pgStores.Teams, d.workspace, d.msgBus))
	}
	if h.builtinTools != nil {
		d.server.SetBuiltinToolsHandler(h.builtinTools)
	}
	if h.pendingMessages != nil {
		if pc := d.cfg.Channels.PendingCompaction; pc != nil {
			h.pendingMessages.SetKeepRecent(pc.KeepRecent)
			h.pendingMessages.SetMaxTokens(pc.MaxTokens)
			h.pendingMessages.SetProviderModel(pc.Provider, pc.Model)
		}
		d.server.SetPendingMessagesHandler(h.pendingMessages)
	}
	if h.secureCLI != nil {
		d.server.SetSecureCLIHandler(h.secureCLI)
	}
	if h.secureCLIGrant != nil {
		d.server.SetSecureCLIGrantHandler(h.secureCLIGrant)
	}

	// Activity audit log API
	if d.pgStores.Activity != nil {
		d.server.SetActivityHandler(httpapi.NewActivityHandler(d.pgStores.Activity))
	}

	// Inbox API (extension badge + unread list + mark-read + reply drafting).
	inboxH := httpapi.NewInboxHandler("http://composio-mcp:9300", d.providerRegistry, d.pgStores.SystemConfigs)
	// Push half: Composio email triggers → /v1/webhooks/composio → WS event.
	// Enabled only when the signing secret is present (set COMPOSIO_WEBHOOK_SECRET
	// + the project webhook URL in the Composio dashboard); otherwise polling-only.
	if secret := os.Getenv("COMPOSIO_WEBHOOK_SECRET"); secret != "" && d.pgStores.Tenants != nil {
		inboxH.EnablePush(secret, d.msgBus, d.pgStores.Tenants)
	}
	d.server.SetInboxHandler(inboxH)

	// System configs API
	if d.pgStores.SystemConfigs != nil {
		d.server.SetSystemConfigsHandler(httpapi.NewSystemConfigsHandler(d.pgStores.SystemConfigs, d.msgBus))

		// Refresh in-memory config when system_configs change via HTTP API
		d.msgBus.Subscribe(bus.TopicSystemConfigChanged, func(evt bus.Event) {
			// Use tenant context from the request that triggered the change
			ctx := context.Background()
			if reqCtx, ok := evt.Payload.(context.Context); ok {
				ctx = reqCtx
			} else {
				ctx = store.WithTenantID(ctx, store.MasterTenantID)
			}
			if sysConfigs, err := d.pgStores.SystemConfigs.List(ctx); err == nil && len(sysConfigs) > 0 {
				d.cfg.ApplySystemConfigs(sysConfigs)
				// Update PGMemoryStore chunk config so new documents use updated settings
				if mem := d.cfg.Agents.Defaults.Memory; mem != nil {
					if pgMem, ok := d.pgStores.Memory.(*pg.PGMemoryStore); ok {
						pgMem.UpdateChunkConfig(mem.MaxChunkLen, mem.ChunkOverlap)
					}
				}
				// Note: vault enrichment provider is resolved per-tenant at runtime,
				// no hot-reload needed here
				slog.Debug("system_configs refreshed to in-memory config", "keys", len(sysConfigs))
			}
		})
	}

	// Usage analytics API
	if d.pgStores.Snapshots != nil {
		d.server.SetUsageHandler(httpapi.NewUsageHandler(d.pgStores.Snapshots, d.pgStores.DB))
	}

	// Runtime package management (install/uninstall system/pip/npm/github packages)
	initGitHubInstaller()
	d.server.SetPackagesHandler(httpapi.NewPackagesHandler())

	// API documentation (OpenAPI spec + Swagger UI at /docs)
	d.server.SetDocsHandler(httpapi.NewDocsHandler())

	// Edition info (public, no auth — used by desktop UI comparison modal)
	d.server.SetEditionHandler(httpapi.NewEditionHandler())

	if d.pgStores != nil && d.pgStores.APIKeys != nil {
		d.server.SetAPIKeysHandler(httpapi.NewAPIKeysHandler(d.pgStores.APIKeys, d.msgBus))
		d.server.SetAPIKeyStore(d.pgStores.APIKeys)
		httpapi.InitAPIKeyCache(d.pgStores.APIKeys, d.msgBus)
	}

	// Allow browser-paired users to access HTTP APIs
	if d.pgStores.Pairing != nil {
		httpapi.InitPairingAuth(d.pgStores.Pairing)
	}

	// Memory management API
	if d.pgStores != nil && d.pgStores.Memory != nil {
		d.server.SetMemoryHandler(httpapi.NewMemoryHandler(d.pgStores.Memory))
	}

	// Knowledge graph API
	if d.pgStores != nil && d.pgStores.KnowledgeGraph != nil {
		d.server.SetKnowledgeGraphHandler(httpapi.NewKnowledgeGraphHandler(d.pgStores.KnowledgeGraph, d.providerRegistry))
	}

	// V3: Evolution metrics + suggestions API
	if d.pgStores != nil && d.pgStores.EvolutionMetrics != nil && d.pgStores.EvolutionSuggestions != nil {
		var evoOpts []httpapi.EvolutionHandlerOpt
		if manageStore, ok := d.pgStores.Skills.(store.SkillManageStore); ok && d.skillsLoader != nil {
			evoOpts = append(evoOpts, httpapi.WithSkillCreation(manageStore, d.skillsLoader, d.dataDir))
		}
		if d.pgStores.Agents != nil {
			evoOpts = append(evoOpts, httpapi.WithAgentStore(d.pgStores.Agents))
		}
		if d.pgStores.BuiltinToolTenantCfgs != nil {
			evoOpts = append(evoOpts, httpapi.WithToolTenantCfgs(d.pgStores.BuiltinToolTenantCfgs))
		}
		d.server.SetEvolutionHandler(httpapi.NewEvolutionHandler(d.pgStores.EvolutionMetrics, d.pgStores.EvolutionSuggestions, evoOpts...))
	}

	// V3: Knowledge Vault document API
	if d.pgStores != nil && d.pgStores.Vault != nil {
		vh := httpapi.NewVaultHandler(d.pgStores.Vault, d.pgStores.Teams, d.workspace, d.domainBus, d.pgStores.Agents, d.pgStores.Teams)
		vh.SetEnrichProgress(d.enrichProgress)
		vh.SetEnrichWorker(d.enrichWorker)
		d.server.SetVaultHandler(vh)

		// Lightweight graph visualization endpoints (vault + KG).
		var kgGraph store.KGGraphStore
		if d.pgStores.KnowledgeGraph != nil {
			kgGraph = newKGGraphStore(d.pgStores.DB)
		}
		vgHandler := httpapi.NewVaultGraphHandler(
			newVaultGraphStore(d.pgStores.DB), kgGraph, d.pgStores.Teams,
		)
		d.server.SetVaultGraphHandler(vgHandler)
	}

	// V3: Episodic memory summaries API
	if d.pgStores != nil && d.pgStores.Episodic != nil {
		d.server.SetEpisodicHandler(httpapi.NewEpisodicHandler(d.pgStores.Episodic))
	}

	// V3: Orchestration mode API (read-only)
	if d.pgStores != nil && d.pgStores.Agents != nil {
		d.server.SetOrchestrationHandler(httpapi.NewOrchestrationHandler(d.pgStores.Agents, d.pgStores.Teams, d.pgStores.AgentLinks))
	}

	// V3: Per-agent v3 feature flags API
	if d.pgStores != nil && d.pgStores.Agents != nil {
		d.server.SetV3FlagsHandler(httpapi.NewV3FlagsHandler(d.pgStores.Agents))
	}

	// Workspace file serving endpoint — serves files by absolute path, auth-token protected.
	// mediaStore is passed in so the handler can re-hydrate .media-cache/ paths
	// from S3 when the local copy is missing (cache wipe, ASG bounce, sibling
	// instance). nil store falls back to the legacy local-only behavior.
	d.server.SetFilesHandler(httpapi.NewFilesHandler(d.workspace, d.dataDir, mediaStore))

	// sheet.preview WS RPC — parses a delivered .xlsx/.csv (same workspace/data
	// bounds as the file server) into a JSON grid the chat UI renders inline.
	methods.NewSheetPreviewMethods(d.workspace, d.dataDir, d.providerRegistry, d.pgStores.SystemConfigs).Register(d.server.Router())

	// Storage file management — browse/delete files under the resolved workspace directory.
	d.server.SetStorageHandler(httpapi.NewStorageHandler(d.workspace))

	// Media upload endpoint — streams multipart uploads straight into the
	// configured MediaStore (S3-backed in prod). Returns a UUID-shaped
	// cache path that the chat.send boundary normalizer can resolve on
	// any sibling instance via mediastore.ResolveLocalPath shape 3.
	d.server.SetMediaUploadHandler(httpapi.NewMediaUploadHandler(mediaStore))

	// Media serve endpoint — serves persisted media files by ID for WS/web clients.
	if mediaStore != nil {
		d.server.SetMediaServeHandler(httpapi.NewMediaServeHandler(mediaStore))
		// Internal media-import endpoint — lets document-mcp and future
		// internal services push generated artefacts into MediaStore via
		// the gateway bearer token instead of holding S3 creds + a private
		// S3 prefix per service. Returned cache path goes through
		// SignMediaPath on every history fetch, so chat-embedded links
		// stay live for as long as the bucket lifecycle keeps the object.
		d.server.SetMediaImportHandler(httpapi.NewMediaImportHandler(mediaStore))
	}

	// Sheet Workflows — orchestrator runtime + internal enqueue endpoint.
	// Wires together: PG store (workflow + run + cell state), tenant-aware
	// LLMCellExecutor (provider resolved per tenant via the same registry
	// chat sessions use), MCPSheetWriter (POSTs sheets_batch_update to the
	// sheets-mcp sidecar per wave flush), BusEventBus (forwards run /
	// cell events onto the same WS bus the SPA already subscribes to).
	//
	// Workflows are disabled by default — only spin up when the DB is
	// configured. The writer drives composio-mcp's
	// GOOGLESHEETS_VALUES_UPDATE per cell (best practice: piggyback on
	// the user's Composio Google OAuth, no duplicate connect prompt).
	if d.pgStores != nil && d.pgStores.DB != nil {
		workflowStore := pg.NewPGSheetWorkflowStore(d.pgStores.DB)

		// Resolve provider + model via the same path background workers
		// use (dreaming / episodic / vault enrich consolidation). This
		// honours system_configs background.provider / background.model
		// overrides, falls back to agent.default_*, and ultimately to
		// the ai_models alias chain — no hardcoded model name, no
		// hardcoded provider name.
		registry := d.providerRegistry
		sysCfgs := d.pgStores.SystemConfigs
		llmExec := runtime.NewLLMCellExecutorTenant(
			func(ctx context.Context, tenantID uuid.UUID) (providers.Provider, string, error) {
				p, m := providerresolve.ResolveBackgroundProvider(ctx, tenantID, registry, sysCfgs)
				if p == nil {
					return nil, "", fmt.Errorf("no background provider for tenant %s", tenantID)
				}
				return p, m, nil
			},
			d.pgStores.Tenants,
		)

		// Wire the per-cell web_search hook. Reuses the same tool the
		// agent loop already has registered (Brave / Tavily / Serper /
		// DDG fallback chain — see internal/tools/web_search.go). When
		// the tool isn't registered (no provider configured) we leave
		// the executor in pure-training mode by skipping SetWebSearch.
		if d.toolsReg != nil {
			if t, ok := d.toolsReg.Get("web_search"); ok {
				llmExec.SetWebSearch(cellSearchAdapter{tool: t})
			}
		}

		// composio-mcp lives on the docker internal network at a fixed
		// host (no env override — it's a same-stack sidecar). Auth is
		// per-call via X-Proxy-User; no shared service token.
		composioURL := "http://composio-mcp:9300"
		writer := runtime.NewMCPSheetWriter(composioURL, "" /*legacy token unused*/, "")
		evtBus := runtime.NewBusEventBus(d.msgBus)
		orch := runtime.New(workflowStore, llmExec, evtBus, writer)
		orch.SetMaxConcurrent(d.cfg.Workflows.MaxConcurrent)

		// MCP tools (sheets_enrich_run) know only the user_id; this
		// resolver looks up the tenant via tenant_users so the agent
		// doesn't need to plumb tenant_id through the tool call.
		// Picks the first (oldest) tenant for the user — multi-tenant
		// users (rare) can pass tenant_id explicitly to override.
		db := d.pgStores.DB
		resolveTenant := func(ctx context.Context, userID string) (uuid.UUID, error) {
			var tid uuid.UUID
			err := db.QueryRowContext(ctx,
				`SELECT tenant_id FROM tenant_users
				 WHERE user_id = $1
				 ORDER BY created_at ASC
				 LIMIT 1`, userID).Scan(&tid)
			if err != nil {
				return uuid.Nil, fmt.Errorf("tenant_users lookup user_id=%s: %w", userID, err)
			}
			return tid, nil
		}
		enqueueH := httpapi.NewWorkflowEnqueueHandler(workflowStore, orch).
			WithTenantResolver(resolveTenant).
			WithTenantStore(d.pgStores.Tenants)
		d.server.SetWorkflowEnqueueHandler(enqueueH)

		// SPA-facing WS methods:
		//   workflow.runState  — reconnect rehydration of the canvas
		//                        grid status (read-only).
		//   workflow.enqueue   — Enrich wizard kicks off a new run on
		//                        behalf of the WS-authenticated user.
		//   workflow.peekSheet — reads actual Google Sheet values via
		//                        the user's own composio OAuth; used
		//                        by the chat-bubble grid so values
		//                        come from the source of truth (the
		//                        sheet) rather than goclaw's DB cache.
		// All three share the same tenant scoping as chat.* — caller's
		// WS session is authoritative, client-supplied tenant_id ignored.
		reader := runtime.NewMCPSheetReader(composioURL)
		// evtBus is the same instance the orchestrator publishes to —
		// passing it to WorkflowMethods lets workflow.runsSubscribe
		// read from its per-run resume ring buffer.
		methods.NewWorkflowMethods(workflowStore, enqueueH, reader, evtBus).Register(d.server.Router())

		slog.Info("workflows orchestrator wired",
			"writer", "composio-mcp",
			"reader", "composio-mcp",
			"resolver", "providerresolve.ResolveBackgroundProvider",
			"ws_methods", "workflow.runState, workflow.enqueue, workflow.peekSheet, workflow.runsSubscribe",
		)
	} else {
		slog.Info("workflows orchestrator disabled (no PG store)")
	}

	// ElevenLabs voice list + refresh endpoints (GET /v1/voices, POST /v1/voices/refresh).
	// VoiceCache is shared between the HTTP handler and the WS voices.list method.
	// TTL 1h + LRU cap 1000 tenants.
	{
		voiceCache := audio.NewVoiceCache(1*time.Hour, 1000)
		var secretStore store.ConfigSecretsStore
		if d.pgStores != nil && d.pgStores.ConfigSecrets != nil {
			secretStore = d.pgStores.ConfigSecrets
		}
		var tenantStore store.TenantStore
		if d.pgStores != nil && d.pgStores.Tenants != nil {
			tenantStore = d.pgStores.Tenants
		}
		voicesH := httpapi.NewVoicesHandler(voiceCache, secretStore, tenantStore)
		d.server.SetVoicesHandler(voicesH)
		// Wire WS method — provider nil means each request resolves key via secretStore at HTTP layer.
		// For WS, use same cache. Provider is resolved via secretStore at WS level in a future phase.
		methods.NewVoicesMethods(voiceCache, nil).Register(d.server.Router())
	}

	// TTS synthesize endpoint — shares audio.Manager with setupTTS.
	if d.audioMgr != nil {
		ttsH := httpapi.NewTTSHandler(d.audioMgr)
		// Reuse the server's rate limiter (per-IP/token; NOT per-user).
		// Server.RateLimiter() is non-nil by construction (server.go:104).
		if rl := d.server.RateLimiter(); rl != nil && rl.Enabled() {
			ttsH.SetRateLimiter(rl.Allow)
		}
		// Wire stores for per-tenant TTS config lookup at synthesis time.
		if d.pgStores.SystemConfigs != nil && d.pgStores.ConfigSecrets != nil {
			ttsH.SetStores(d.pgStores.SystemConfigs, d.pgStores.ConfigSecrets)
			// Wire tenant resolver for channels TTS auto-apply
			d.audioMgr.SetTenantResolver(httpapi.NewTenantTTSResolver(d.pgStores.SystemConfigs, d.pgStores.ConfigSecrets))
		}
		d.server.SetTTSHandler(ttsH)
		d.ttsHandler = ttsH // store for hot-reload
	}

	// Per-tenant TTS config endpoint — allows tenant admins to configure TTS.
	if d.pgStores.SystemConfigs != nil && d.pgStores.ConfigSecrets != nil {
		d.server.SetTTSConfigHandler(httpapi.NewTTSConfigHandler(d.pgStores.SystemConfigs, d.pgStores.ConfigSecrets))
	}

	// Seed + apply builtin tool disables
	if d.pgStores.BuiltinTools != nil {
		seedBuiltinTools(context.Background(), d.pgStores.BuiltinTools)
		migrateBuiltinToolSettings(context.Background(), d.pgStores.BuiltinTools)
		backfillWebFetchSettings(context.Background(), d.pgStores.BuiltinTools)
		applyBuiltinToolDisables(context.Background(), d.pgStores.BuiltinTools, d.toolsReg)
	}
}

// cellSearchAdapter wraps the canonical web_search tool so it satisfies
// runtime.CellWebSearch. Kept here (cmd/) rather than in workflow/runtime/
// to avoid importing internal/tools from the runtime package and the
// circular-dep risk that comes with it.
type cellSearchAdapter struct {
	tool tools.Tool
}

func (a cellSearchAdapter) Search(ctx context.Context, query string) string {
	if a.tool == nil || query == "" {
		return ""
	}
	res := a.tool.Execute(ctx, map[string]any{"query": query})
	if res == nil {
		return ""
	}
	return res.ForLLM
}
