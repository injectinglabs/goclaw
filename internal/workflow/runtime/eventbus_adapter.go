package runtime

import (
	"context"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
)

// BusEventBus adapts the orchestrator's RunEvent stream onto goclaw's
// existing bus.EventPublisher (which the WS gateway already subscribes
// to via internal/gateway/event_filter.go).
//
// The event name is fixed as "workflow.event" and the per-RunEvent
// `type` field carries the actual sub-kind (run.started / cell.update
// / etc). Tenant scoping is honoured so the WS event filter doesn't
// leak cross-tenant.
type BusEventBus struct {
	pub bus.EventPublisher
}

// NewBusEventBus wraps an EventPublisher with the runtime.EventBus
// interface. Pass the same publisher used by the gateway server so
// SPA clients see workflow events on the same bus they already use
// for chat / channels.
func NewBusEventBus(pub bus.EventPublisher) *BusEventBus {
	return &BusEventBus{pub: pub}
}

// PublishWorkflowEvent forwards a RunEvent onto the bus. Implements
// runtime.EventBus.
func (b *BusEventBus) PublishWorkflowEvent(_ context.Context, ev RunEvent) {
	if b == nil || b.pub == nil {
		return
	}
	bus.BroadcastForTenant(b.pub, "workflow.event", ev.TenantID, ev)
}
