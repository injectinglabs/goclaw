package bus

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

// pgCacheChannel is the Postgres NOTIFY channel used to mirror cache
// invalidations across replicas.
const pgCacheChannel = "goclaw_cache_invalidate"

// wireCacheEvent is the JSON payload carried over NOTIFY. Kept small —
// Postgres caps NOTIFY payloads at 8000 bytes.
type wireCacheEvent struct {
	Origin   string    `json:"o"`   // emitting process id; receivers skip their own
	Name     string    `json:"n"`   // event name (protocol.EventCacheInvalidate)
	Kind     string    `json:"k"`   // CacheKind*
	Key      string    `json:"key"` // entity id/key, "" = invalidate all
	TenantID uuid.UUID `json:"t"`
}

// StartPGCacheBridge makes cache-invalidation events cross replica boundaries.
//
// The in-process MessageBus remains the local fan-out; this adds a Postgres
// LISTEN/NOTIFY mirror so an invalidation emitted on one replica (e.g. a
// channel disconnect handled by replica A) reaches every other replica. Without
// it, a multi-replica deployment leaves peers with a stale channel registry —
// the bot keeps replying after a disconnect handled elsewhere.
//
// originID identifies this process so it ignores the echo of its own NOTIFYs
// (it already delivered them locally via Broadcast). The listener auto-reconnects
// via lib/pq; if it can't connect the bridge degrades to local-only and logs.
func StartPGCacheBridge(ctx context.Context, db *sql.DB, dsn, originID string, mb *MessageBus) error {
	if db == nil || dsn == "" {
		return fmt.Errorf("cache bridge: db and dsn required")
	}

	// Notifier: every cache-invalidation broadcast also NOTIFYs peers. Uses the
	// pooled *sql.DB — pg_notify works on any connection. A short timeout keeps
	// a slow NOTIFY from ever blocking the caller's Broadcast.
	mb.SetDistributor(func(e Event) {
		p, ok := e.Payload.(CacheInvalidatePayload)
		if !ok {
			return
		}
		payload, err := json.Marshal(wireCacheEvent{
			Origin: originID, Name: e.Name, Kind: p.Kind, Key: p.Key, TenantID: p.TenantID,
		})
		if err != nil {
			slog.Warn("cache bridge: marshal notify payload", "err", err)
			return
		}
		nctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if _, err := db.ExecContext(nctx, "select pg_notify($1, $2)", pgCacheChannel, string(payload)); err != nil {
			slog.Warn("cache bridge: pg_notify failed", "err", err)
		}
	})

	listener := pq.NewListener(dsn, time.Second, time.Minute, func(_ pq.ListenerEventType, err error) {
		if err != nil {
			slog.Warn("cache bridge: listener connection event", "err", err)
		}
	})
	if err := listener.Listen(pgCacheChannel); err != nil {
		_ = listener.Close()
		mb.SetDistributor(nil) // don't NOTIFY into a channel nobody on this box listens to
		return fmt.Errorf("cache bridge: listen %q: %w", pgCacheChannel, err)
	}

	go runCacheListener(ctx, listener, originID, mb)
	slog.Info("cache invalidation bridge started", "transport", "postgres LISTEN/NOTIFY", "origin", originID)
	return nil
}

// runCacheListener replays peers' invalidations onto the local bus until ctx is done.
func runCacheListener(ctx context.Context, listener *pq.Listener, originID string, mb *MessageBus) {
	defer func() { _ = listener.Close() }()
	// Periodic Ping lets lib/pq detect a dead connection and reconnect during
	// quiet periods (no NOTIFYs to surface a broken socket otherwise).
	ping := time.NewTicker(90 * time.Second)
	defer ping.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case n := <-listener.Notify:
			if n == nil {
				// nil = reconnect occurred; lib/pq re-LISTENs automatically.
				continue
			}
			var w wireCacheEvent
			if err := json.Unmarshal([]byte(n.Extra), &w); err != nil {
				slog.Warn("cache bridge: bad notify payload", "extra", n.Extra, "err", err)
				continue
			}
			if w.Origin == originID {
				continue // our own echo — already delivered locally
			}
			mb.BroadcastLocal(Event{
				Name:     w.Name,
				Payload:  CacheInvalidatePayload{Kind: w.Kind, Key: w.Key, TenantID: w.TenantID},
				TenantID: w.TenantID,
			})
		case <-ping.C:
			go func() { _ = listener.Ping() }()
		}
	}
}
