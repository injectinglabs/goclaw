package runtime

import (
	"context"
	"sync"

	"github.com/google/uuid"

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
//
// Resume buffer: every emitted event is stamped with a monotonic per-
// run Seq and kept in a bounded in-memory ring (one ring per run id).
// On WS reconnect the SPA calls workflow.runsSubscribe(run_id,
// since_seq); EventsSince returns the missed tail so reload-after-
// disconnect restores the chip's grid without waiting on the next
// Sheet peek. Same shape as agent.Router.EventsSince used by
// runs.subscribe — identical contract, separate buffer.
type BusEventBus struct {
	pub bus.EventPublisher

	mu      sync.Mutex
	buffers map[uuid.UUID]*workflowRunBuffer
}

// workflowRunBuffer is the per-run resume ring used by EventsSince to
// replay events the SPA missed during a WS disconnect.
type workflowRunBuffer struct {
	nextSeq int64
	events  []RunEvent
}

// maxWorkflowRunEventLog caps each run's resume buffer. For a 500-cell
// run that's ~1500 events (queued+running+done per cell + a few
// progress flushes); 2000 is comfortably above that ceiling. Ring
// drops oldest so unbounded growth from a stuck/recovered run never
// pins memory.
const maxWorkflowRunEventLog = 2000

// NewBusEventBus wraps an EventPublisher with the runtime.EventBus
// interface. Pass the same publisher used by the gateway server so
// SPA clients see workflow events on the same bus they already use
// for chat / channels.
func NewBusEventBus(pub bus.EventPublisher) *BusEventBus {
	return &BusEventBus{
		pub:     pub,
		buffers: map[uuid.UUID]*workflowRunBuffer{},
	}
}

// PublishWorkflowEvent stamps Seq, appends to the per-run resume
// buffer, and forwards on the tenant-scoped bus. Implements
// runtime.EventBus. Safe for concurrent callers (orchestrator emits
// from per-cell worker goroutines).
func (b *BusEventBus) PublishWorkflowEvent(_ context.Context, ev RunEvent) {
	if b == nil || b.pub == nil {
		return
	}
	b.mu.Lock()
	buf, ok := b.buffers[ev.RunID]
	if !ok {
		buf = &workflowRunBuffer{}
		b.buffers[ev.RunID] = buf
	}
	buf.nextSeq++
	ev.Seq = buf.nextSeq
	buf.events = append(buf.events, ev)
	if len(buf.events) > maxWorkflowRunEventLog {
		// Ring drop: copy the tail into a fresh slice so dropped
		// events become eligible for GC instead of pinning the
		// backing array via the slice header.
		drop := len(buf.events) - maxWorkflowRunEventLog
		next := make([]RunEvent, maxWorkflowRunEventLog)
		copy(next, buf.events[drop:])
		buf.events = next
	}
	// Evict on terminal — keep a short tail (~10 events) so a
	// resubscribe that races with run.completed still picks up the
	// final state, but drop the bulk so completed runs don't pin
	// memory indefinitely.
	terminal := ev.Type == "run.completed" || ev.Type == "run.error"
	if terminal && len(buf.events) > 10 {
		tail := make([]RunEvent, 10)
		copy(tail, buf.events[len(buf.events)-10:])
		buf.events = tail
	}
	b.mu.Unlock()

	bus.BroadcastForTenant(b.pub, "workflow.event", ev.TenantID, ev)
}

// EventsSince returns buffered events for runID whose Seq > sinceSeq,
// in emit order. Returns nil if the run is unknown (never buffered or
// already evicted). Callers must NOT mutate the returned slice — it
// is a defensive copy.
//
// Used by the workflow.runsSubscribe WS method: client supplies its
// last-seen Seq, server replies with everything newer; live broadcast
// continues to fan out subsequent events as they happen.
func (b *BusEventBus) EventsSince(runID uuid.UUID, sinceSeq int64) []RunEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	buf, ok := b.buffers[runID]
	if !ok || len(buf.events) == 0 {
		return nil
	}
	out := make([]RunEvent, 0, len(buf.events))
	for _, e := range buf.events {
		if e.Seq > sinceSeq {
			out = append(out, e)
		}
	}
	return out
}
