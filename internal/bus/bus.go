package bus

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
)

// MessageBus routes messages between channels and the agent runtime,
// and broadcasts events to WebSocket subscribers.
type MessageBus struct {
	inbound  chan InboundMessage
	outbound chan OutboundMessage

	// Channel message handlers (channel name → handler)
	handlers map[string]MessageHandler
	handlerMu sync.RWMutex

	// Event subscribers (subscriber ID → handler)
	subscribers map[string]EventHandler
	subMu       sync.RWMutex

	// distributor, if set, mirrors cache-invalidation events to peer replicas
	// (e.g. via Postgres LISTEN/NOTIFY). Guarded by subMu. nil in single-process
	// deployments (desktop/SQLite), where the in-process fan-out is sufficient.
	distributor func(Event)
}

func New() *MessageBus {
	return &MessageBus{
		inbound:     make(chan InboundMessage, 1000),
		outbound:    make(chan OutboundMessage, 1000),
		handlers:    make(map[string]MessageHandler),
		subscribers: make(map[string]EventHandler),
	}
}

// PublishInbound queues an inbound message from a channel.
// Blocks if the inbound buffer is full.
func (mb *MessageBus) PublishInbound(msg InboundMessage) {
	mb.inbound <- msg
}

// TryPublishInbound attempts to queue an inbound message without blocking.
// Returns false if the inbound buffer is full (message dropped).
func (mb *MessageBus) TryPublishInbound(msg InboundMessage) bool {
	select {
	case mb.inbound <- msg:
		return true
	default:
		return false
	}
}

// ConsumeInbound blocks until an inbound message is available or ctx is cancelled.
func (mb *MessageBus) ConsumeInbound(ctx context.Context) (InboundMessage, bool) {
	select {
	case msg := <-mb.inbound:
		return msg, true
	case <-ctx.Done():
		return InboundMessage{}, false
	}
}

// PublishOutbound queues an outbound message to a channel.
// Blocks if the outbound buffer is full.
func (mb *MessageBus) PublishOutbound(msg OutboundMessage) {
	mb.outbound <- msg
}

// TryPublishOutbound attempts to queue an outbound message without blocking.
// Returns false if the outbound buffer is full (message dropped).
func (mb *MessageBus) TryPublishOutbound(msg OutboundMessage) bool {
	select {
	case mb.outbound <- msg:
		return true
	default:
		return false
	}
}

// SubscribeOutbound blocks until an outbound message is available or ctx is cancelled.
func (mb *MessageBus) SubscribeOutbound(ctx context.Context) (OutboundMessage, bool) {
	select {
	case msg := <-mb.outbound:
		return msg, true
	case <-ctx.Done():
		return OutboundMessage{}, false
	}
}

// RegisterHandler registers a message handler for a channel.
func (mb *MessageBus) RegisterHandler(channel string, handler MessageHandler) {
	mb.handlerMu.Lock()
	defer mb.handlerMu.Unlock()
	mb.handlers[channel] = handler
}

// GetHandler returns the message handler for a channel.
func (mb *MessageBus) GetHandler(channel string) (MessageHandler, bool) {
	mb.handlerMu.RLock()
	defer mb.handlerMu.RUnlock()
	handler, ok := mb.handlers[channel]
	return handler, ok
}

// Subscribe registers an event subscriber. Returns the subscriber ID for unsubscribe.
func (mb *MessageBus) Subscribe(id string, handler EventHandler) {
	mb.subMu.Lock()
	defer mb.subMu.Unlock()
	mb.subscribers[id] = handler
}

// Unsubscribe removes an event subscriber.
func (mb *MessageBus) Unsubscribe(id string) {
	mb.subMu.Lock()
	defer mb.subMu.Unlock()
	delete(mb.subscribers, id)
}

// SetDistributor installs (or clears, with nil) the peer-replica distributor.
// Called once at startup by the Postgres cache bridge. Safe to leave unset.
func (mb *MessageBus) SetDistributor(fn func(Event)) {
	mb.subMu.Lock()
	defer mb.subMu.Unlock()
	mb.distributor = fn
}

// Broadcast delivers an event to all in-process subscribers and, for
// cache-invalidation events, also mirrors it to peer replicas via the
// distributor (if one is installed). Distribution is gated on the payload
// being a CacheInvalidatePayload so high-frequency UI events (agent/chat
// tokens) are never sent over the wire.
func (mb *MessageBus) Broadcast(event Event) {
	dist := mb.broadcastLocal(event)
	if dist == nil {
		return
	}
	if _, ok := event.Payload.(CacheInvalidatePayload); ok {
		dist(event)
	}
}

// BroadcastLocal delivers an event only to in-process subscribers, without
// re-distributing it to peers. The Postgres cache bridge uses this when
// replaying a peer's invalidation, so it cannot echo back onto the wire.
func (mb *MessageBus) BroadcastLocal(event Event) {
	mb.broadcastLocal(event)
}

// broadcastLocal fans an event out to in-process subscribers and returns the
// currently-installed distributor (read under the same lock). Panicking
// handlers are caught and logged so one bad subscriber can't crash the bus.
func (mb *MessageBus) broadcastLocal(event Event) func(Event) {
	mb.subMu.RLock()
	defer mb.subMu.RUnlock()
	for id, handler := range mb.subscribers {
		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("bus: subscriber panicked",
						"subscriber", id,
						"event", event.Name,
						"panic", fmt.Sprint(r),
					)
				}
			}()
			handler(event)
		}()
	}
	return mb.distributor
}

// Close shuts down the message bus.
func (mb *MessageBus) Close() {
	close(mb.inbound)
	close(mb.outbound)
}
