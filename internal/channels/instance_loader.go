package channels

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/config"
	"github.com/nextlevelbuilder/goclaw/internal/providerresolve"
	"github.com/nextlevelbuilder/goclaw/internal/providers"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// reloadStartTimeout bounds how long Reload() will wait for a single channel's
// Start() to return before abandoning it and continuing with the rest.
// Sits above Telegram's probeOverallTimeout (60s) so well-behaved channels
// always get a chance to finish before being given up on.
// var (not const) so tests can shrink it without waiting a real minute+.
var reloadStartTimeout = 90 * time.Second

// reloadStopTimeout bounds how long Reload()/RestartInstance() will wait for
// a single channel's Stop() before abandoning it. A hung Stop in the upstream
// channel implementation must not freeze the entire reload loop and starve
// every other tenant. 30s sits above Telegram's internal pollDone+handlerWg
// budget (10s+15s).
var reloadStopTimeout = 30 * time.Second

// ChannelFactory creates a Channel from DB instance data.
// name: channel name (registered in Manager, used in session keys).
// creds: decrypted credentials JSON (token, API keys, etc.).
// cfg: non-secret config JSONB (dm_policy, dm_stream, group_stream, etc.).
type ChannelFactory func(name string, creds json.RawMessage, cfg json.RawMessage,
	msgBus *bus.MessageBus, pairingSvc store.PairingStore) (Channel, error)

// InstanceLoader loads channel instances from the database and registers them with the Manager.
// Follows a load-all-at-startup pattern with cache invalidation for reload.
type InstanceLoader struct {
	store             store.ChannelInstanceStore
	agentStore        store.AgentStore
	providerReg       *providers.Registry
	pendingCompactCfg *config.PendingCompactionConfig
	tenantStore       store.TenantStore // for outbound actor-header attribution on pending compaction
	factories         map[string]ChannelFactory
	manager           *Manager
	msgBus            *bus.MessageBus
	pairingSvc        store.PairingStore
	mu                sync.Mutex
	loaded            map[string]struct{} // channel names managed by this loader
}

// NewInstanceLoader creates a new InstanceLoader.
func NewInstanceLoader(
	s store.ChannelInstanceStore,
	agentStore store.AgentStore,
	mgr *Manager,
	msgBus *bus.MessageBus,
	pairingSvc store.PairingStore,
) *InstanceLoader {
	return &InstanceLoader{
		store:      s,
		agentStore: agentStore,
		factories:  make(map[string]ChannelFactory),
		manager:    mgr,
		msgBus:     msgBus,
		pairingSvc: pairingSvc,
		loaded:     make(map[string]struct{}),
	}
}

// SetProviderRegistry sets the provider registry for pending message compaction.
// Must be called before LoadAll/Reload.
func (l *InstanceLoader) SetProviderRegistry(reg *providers.Registry) {
	l.providerReg = reg
}

// SetPendingCompactionConfig sets the global pending message compaction thresholds.
// Must be called before LoadAll/Reload.
func (l *InstanceLoader) SetPendingCompactionConfig(cfg *config.PendingCompactionConfig) {
	l.pendingCompactCfg = cfg
}

// SetTenantStore wires the tenant store so per-channel compaction can
// resolve X-Actor-Org-ID for outbound LLM calls. Optional — when nil
// the compaction call lands without actor headers and 400's at the
// receiver, surfacing as a clear "this channel isn't wired" signal.
func (l *InstanceLoader) SetTenantStore(ts store.TenantStore) {
	l.tenantStore = ts
}

// RegisterFactory registers a factory for a channel type (e.g., "telegram", "discord").
func (l *InstanceLoader) RegisterFactory(channelType string, factory ChannelFactory) {
	l.factories[channelType] = factory
}

// LoadAll loads all enabled channel instances from the database, creates channels, and registers them.
func (l *InstanceLoader) LoadAll(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	instances, err := l.store.ListAllEnabled(ctx)
	if err != nil {
		return err
	}

	registered := 0
	for _, inst := range instances {
		// Don't start channels here — StartAll() will start them after all channels are registered.
		if err := l.loadInstance(ctx, inst, false); err != nil {
			slog.Error("failed to load channel instance",
				"name", inst.Name, "type", inst.ChannelType, "error", err)
			continue
		}
		registered++
	}

	if registered > 0 {
		slog.Info("channel instances loaded from DB", "count", registered)
	}
	return nil
}

// Reload stops all managed channels, reloads from DB, and starts new ones.
// Called on cache invalidation events.
func (l *InstanceLoader) Reload(ctx context.Context) {
	l.mu.Lock()
	defer l.mu.Unlock()

	// Stop and unregister old channels. Each Stop is bounded so a single
	// hung channel impl can't wedge the loop and freeze every other tenant.
	for name := range l.loaded {
		if ch, ok := l.manager.GetChannel(name); ok {
			l.stopChannelWithTimeout(ctx, name, ch)
		}
		l.manager.UnregisterChannel(name)
	}
	l.loaded = make(map[string]struct{})

	// Brief pause to let external APIs (e.g., Telegram getUpdates) release polling locks.
	time.Sleep(500 * time.Millisecond)

	// Reload from DB (all tenants — server-internal)
	instances, err := l.store.ListAllEnabled(ctx)
	if err != nil {
		slog.Error("failed to reload channel instances", "error", err)
		return
	}

	registered := 0
	for _, inst := range instances {
		// Reload must start channels immediately (StartAll was called at boot, not again).
		if err := l.loadInstance(ctx, inst, true); err != nil {
			slog.Error("failed to reload channel instance",
				"name", inst.Name, "type", inst.ChannelType, "error", err)
			continue
		}
		registered++
	}

	slog.Info("channel instances reloaded", "count", registered)
}

// Stop stops all managed channels.
func (l *InstanceLoader) Stop(ctx context.Context) {
	l.mu.Lock()
	defer l.mu.Unlock()

	for name := range l.loaded {
		if ch, ok := l.manager.GetChannel(name); ok {
			l.stopChannelWithTimeout(ctx, name, ch)
		}
		l.manager.UnregisterChannel(name)
	}
	l.loaded = make(map[string]struct{})
}

// RestartInstance stops, reloads, and starts a single channel by ID. Used for
// targeted updates (credentials rotation, enable/disable, config edits) so a
// single-instance change doesn't churn every tenant's bots through the full
// Reload path — and sidesteps any single hung Stop blocking unrelated channels.
//
// If the instance no longer exists or is disabled, the channel (if any) is
// stopped and unregistered without restart.
func (l *InstanceLoader) RestartInstance(ctx context.Context, id uuid.UUID) {
	l.mu.Lock()
	defer l.mu.Unlock()

	// Cross-tenant lookup — server-internal call from the cache invalidation bus.
	inst, err := l.store.Get(store.WithCrossTenant(ctx), id)
	if err != nil {
		slog.Warn("targeted reload: instance not found, skipping",
			"id", id, "error", err)
		return
	}

	// Stop the existing channel for this name, if any.
	if _, alreadyLoaded := l.loaded[inst.Name]; alreadyLoaded {
		if ch, ok := l.manager.GetChannel(inst.Name); ok {
			l.stopChannelWithTimeout(ctx, inst.Name, ch)
		}
		l.manager.UnregisterChannel(inst.Name)
		delete(l.loaded, inst.Name)
	}

	if !inst.Enabled {
		slog.Info("targeted reload: instance disabled, leaving stopped",
			"name", inst.Name, "type", inst.ChannelType)
		return
	}

	// Brief pause so external APIs (Telegram getUpdates) release polling locks.
	time.Sleep(500 * time.Millisecond)

	if err := l.loadInstance(ctx, *inst, true); err != nil {
		slog.Error("targeted reload: failed to load instance",
			"name", inst.Name, "type", inst.ChannelType, "error", err)
		return
	}
	slog.Info("targeted reload: instance restarted",
		"name", inst.Name, "type", inst.ChannelType)
}

// stopChannelWithTimeout runs ch.Stop in a goroutine with a bounded timeout.
// A hung Stop must not block the caller (Reload, RestartInstance, Stop) and
// starve other channels. The late-returning Stop is drained asynchronously so
// its goroutine can eventually exit.
func (l *InstanceLoader) stopChannelWithTimeout(ctx context.Context, name string, ch Channel) {
	stopErr := make(chan error, 1)
	go func() { stopErr <- ch.Stop(ctx) }()

	timer := time.NewTimer(reloadStopTimeout)
	defer timer.Stop()

	select {
	case err := <-stopErr:
		if err != nil {
			slog.Warn("failed to stop channel instance", "name", name, "error", err)
		}
	case <-timer.C:
		slog.Warn("channel stop timed out; abandoning",
			"name", name, "timeout", reloadStopTimeout)
		go func() {
			if err := <-stopErr; err != nil {
				slog.Warn("channel stop returned after timeout",
					"name", name, "error", err)
			}
		}()
	}
}

// coerceStringBools converts string "true"/"false" values to JSON booleans
// in a raw config blob. Older UI versions saved select-based bool fields as strings.
func coerceStringBools(data json.RawMessage) json.RawMessage {
	if len(data) == 0 {
		return data
	}
	var m map[string]any
	if json.Unmarshal(data, &m) != nil {
		return data
	}
	changed := false
	for k, v := range m {
		if s, ok := v.(string); ok {
			switch s {
			case "true":
				m[k] = true
				changed = true
			case "false":
				m[k] = false
				changed = true
			}
		}
	}
	if !changed {
		return data
	}
	out, _ := json.Marshal(m)
	return out
}

// LoadedNames returns the set of channel names managed by the loader.
func (l *InstanceLoader) LoadedNames() map[string]struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()

	result := make(map[string]struct{}, len(l.loaded))
	maps.Copy(result, l.loaded)
	return result
}

// loadInstance creates and registers a single channel from a DB instance (caller must hold lock).
// If autoStart is true, the channel is started immediately (used by Reload).
// If false, the caller is responsible for starting (used by LoadAll, where StartAll handles it).
func (l *InstanceLoader) loadInstance(ctx context.Context, inst store.ChannelInstanceData, autoStart bool) error {
	l.loaded[inst.Name] = struct{}{}

	factory, ok := l.factories[inst.ChannelType]
	if !ok {
		l.manager.RecordHealth(inst.Name, NewChannelHealthForType(
			inst.ChannelType,
			ChannelHealthStateFailed,
			"Unsupported channel type",
			fmt.Sprintf("No channel factory is registered for %q", inst.ChannelType),
			ChannelFailureKindConfig,
			false,
		))
		slog.Warn("no factory for channel type", "type", inst.ChannelType, "name", inst.Name)
		return nil
	}

	// Normalize config: convert string "true"/"false" to JSON booleans.
	// Older UI versions saved select-based bool fields as strings.
	cfg := coerceStringBools(inst.Config)

	ch, err := factory(inst.Name, inst.Credentials, cfg, l.msgBus, l.pairingSvc)
	if err != nil {
		l.manager.RecordFailureForType(inst.Name, inst.ChannelType, "", err)
		return err
	}
	if ch == nil {
		l.manager.RecordHealth(inst.Name, NewChannelHealthForType(
			inst.ChannelType,
			ChannelHealthStateFailed,
			"Missing credentials",
			"Channel instance is enabled but required credentials are incomplete.",
			ChannelFailureKindConfig,
			false,
		))
		slog.Info("channel instance not ready (missing credentials)", "name", inst.Name, "type", inst.ChannelType)
		return nil
	}

	// Resolve agent_key from UUID — the routing system (Router, session keys) uses agent_key, not UUID.
	// Use the instance's tenant_id to scope the agent lookup.
	instCtx := store.WithTenantID(ctx, inst.TenantID)
	var ag *store.AgentData
	if base, ok := ch.(interface{ SetAgentID(string) }); ok {
		var err error
		ag, err = l.agentStore.GetByID(instCtx, inst.AgentID)
		if err != nil {
			l.manager.RecordFailureForType(inst.Name, inst.ChannelType, "", fmt.Errorf("agent %s not found for channel %s: %w", inst.AgentID, inst.Name, err))
			return fmt.Errorf("agent %s not found for channel %s: %w", inst.AgentID, inst.Name, err)
		}
		base.SetAgentID(ag.AgentKey)
	}
	// Set the platform type on the channel so Manager.ChannelTypeForName can read it.
	if base, ok := ch.(interface{ SetType(string) }); ok {
		base.SetType(inst.ChannelType)
	}
	// Propagate tenant_id from DB instance to channel for tenant-scoped message handling.
	if base, ok := ch.(interface{ SetTenantID(uuid.UUID) }); ok {
		base.SetTenantID(inst.TenantID)
	}
	// Propagate created_by (bot owner) for billing attribution. Threaded into
	// bus.InboundMessage on every inbound message so the agent loop can split
	// BILLING (always bot owner) from IDENTITY (linked merged contact).
	if base, ok := ch.(interface{ SetCreatedBy(string) }); ok {
		base.SetCreatedBy(inst.CreatedBy)
	}
	// Propagate the instance UUID so webhook-mode channels can build their
	// per-instance webhook URL/secret and register for inbound routing.
	if base, ok := ch.(interface{ SetInstanceID(uuid.UUID) }); ok {
		base.SetInstanceID(inst.ID)
	}
	// Propagate tenant_id to pending history for compaction/sweep DB operations.
	// Factory creates PendingHistory before SetTenantID is called, so tenantID is uuid.Nil at construction.
	if ph, ok := ch.(interface{ SetPendingHistoryTenantID(uuid.UUID) }); ok {
		ph.SetPendingHistoryTenantID(inst.TenantID)
	}

	// Wire pending message auto-compaction.
	// Priority: config provider/model > agent's provider/model > fallback.
	if pc, ok := ch.(PendingCompactable); ok && l.providerReg != nil {
		var p providers.Provider
		var model string

		// Try config-level provider/model first.
		tctx := store.WithTenantID(ctx, inst.TenantID)
		if l.pendingCompactCfg != nil && l.pendingCompactCfg.Provider != "" {
			if cp, err := l.providerReg.Get(tctx, l.pendingCompactCfg.Provider); err == nil {
				p = cp
				model = l.pendingCompactCfg.Model
				if model == "" {
					model = cp.DefaultModel()
				}
			}
		}
		// Fallback: agent's provider/model.
		if p == nil && ag != nil && ag.Provider != "" {
			if ap, err := providerresolve.ResolveConfiguredProvider(l.providerReg, ag); err == nil {
				p = ap
				model = ag.Model
				if model == "" {
					model = ap.DefaultModel()
				}
			}
		}

		if p != nil && model != "" {
			cc := &CompactionConfig{
				Provider:             p,
				Model:                model,
				TenantStore:          l.tenantStore,
				ChannelInstanceStore: l.store,
			}
			if l.pendingCompactCfg != nil {
				cc.Threshold = l.pendingCompactCfg.Threshold
				cc.KeepRecent = l.pendingCompactCfg.KeepRecent
				cc.MaxTokens = l.pendingCompactCfg.MaxTokens
			}
			pc.SetPendingCompaction(cc)
			slog.Debug("pending compaction configured", "channel", inst.Name, "provider", p.Name(), "model", model,
				"threshold", cc.Threshold, "keep_recent", cc.KeepRecent, "max_tokens", cc.MaxTokens)
		} else {
			attemptedProvider := ""
			if l.pendingCompactCfg != nil {
				attemptedProvider = l.pendingCompactCfg.Provider
			}
			if attemptedProvider == "" && ag != nil {
				attemptedProvider = ag.Provider
			}
			slog.Warn("pending compaction not configured: provider/model unavailable",
				"channel", inst.Name, "agent_id", inst.AgentID, "attempted_provider", attemptedProvider)
		}
	}
	l.manager.RegisterChannel(inst.Name, ch)

	// Start the channel if requested (Reload path). LoadAll defers to StartAll.
	// Bound the wait so one hung Start() can't block Reload()'s mutex and wedge
	// every subsequent reload. Important: we pass the caller's ctx (not a
	// timeout-wrapped one) to ch.Start so long-running goroutines the channel
	// derives from it — e.g. Telegram's pollCtx — are not cancelled out from
	// under a successful start.
	if autoStart {
		l.startChannelWithTimeout(ctx, inst, ch)
	}

	slog.Info("channel instance loaded",
		"name", inst.Name, "type", inst.ChannelType, "agent_id", inst.AgentID)
	return nil
}

// startChannelWithTimeout runs ch.Start(ctx) in a goroutine and waits up to
// reloadStartTimeout for it to return. On timeout we stop the partially-started
// channel and record a failure so Reload() can move on to the next instance.
//
// ctx is passed through unchanged: channels routinely derive long-lived
// goroutines (e.g. Telegram long-polling) from this context and must keep
// running after Start returns. A late-returning Start — i.e. one that ignores
// the caller ctx entirely — is drained asynchronously so its goroutine doesn't
// block forever on the send to startErr. If it eventually reports success,
// we've already called Stop, which is idempotent across channel impls.
func (l *InstanceLoader) startChannelWithTimeout(ctx context.Context, inst store.ChannelInstanceData, ch Channel) {
	startErr := make(chan error, 1)
	go func() { startErr <- ch.Start(ctx) }()

	timer := time.NewTimer(reloadStartTimeout)
	defer timer.Stop()

	select {
	case err := <-startErr:
		if err != nil {
			l.manager.recordChannelStartFailure(inst.Name, ch, "", err)
			slog.Error("channel instance start failed",
				"name", inst.Name, "type", inst.ChannelType, "error", err)
			return
		}
		l.manager.RecordHealth(inst.Name, snapshotChannelHealth(ch))

	case <-timer.C:
		// Stop the channel in a bounded window so we don't trade one hang for another.
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := ch.Stop(stopCtx); err != nil {
			slog.Warn("failed to stop timed-out channel",
				"name", inst.Name, "type", inst.ChannelType, "error", err)
		}
		stopCancel()

		timeoutErr := fmt.Errorf("start timed out after %s (type=%s)", reloadStartTimeout, inst.ChannelType)
		l.manager.recordChannelStartFailure(inst.Name, ch, "", timeoutErr)
		slog.Error("channel instance start timed out",
			"name", inst.Name, "type", inst.ChannelType, "timeout", reloadStartTimeout)

		// Drain the late-returning Start so its goroutine can exit.
		// Logged so operators can spot channels that ignore context cancellation.
		go func() {
			err := <-startErr
			if err != nil {
				slog.Warn("channel instance start returned after timeout",
					"name", inst.Name, "type", inst.ChannelType, "error", err)
				return
			}
			slog.Warn("channel instance start succeeded after timeout; already stopped",
				"name", inst.Name, "type", inst.ChannelType)
		}()
	}
}
