//go:build sqlite || sqliteonly

package sqlitestore

import (
	"context"
	"database/sql"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// SQLitePushSubscriptionStore implements store.PushSubscriptionStore backed by SQLite.
type SQLitePushSubscriptionStore struct {
	db *sql.DB
}

// NewSQLitePushSubscriptionStore creates a new SQLitePushSubscriptionStore.
func NewSQLitePushSubscriptionStore(db *sql.DB) *SQLitePushSubscriptionStore {
	return &SQLitePushSubscriptionStore{db: db}
}

func (s *SQLitePushSubscriptionStore) Upsert(ctx context.Context, sub *store.PushSubscription) error {
	tenantID := sub.TenantID
	if tenantID == uuid.Nil {
		tenantID = store.TenantIDFromContext(ctx)
	}
	if tenantID == uuid.Nil {
		tenantID = store.MasterTenantID
	}
	id := sub.ID
	if id == uuid.Nil {
		id = uuid.New()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO push_subscriptions (id, tenant_id, user_id, endpoint, p256dh, auth)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT (endpoint) DO UPDATE SET
		   tenant_id = excluded.tenant_id,
		   user_id   = excluded.user_id,
		   p256dh    = excluded.p256dh,
		   auth      = excluded.auth`,
		id, tenantID, sub.UserID, sub.Endpoint, sub.P256dh, sub.Auth,
	)
	return err
}

func (s *SQLitePushSubscriptionStore) ListByUser(ctx context.Context, userID string) ([]store.PushSubscription, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, tenant_id, user_id, endpoint, p256dh, auth, created_at
		 FROM push_subscriptions WHERE user_id = ?`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []store.PushSubscription
	for rows.Next() {
		var sub store.PushSubscription
		var createdAt sqliteTime
		if err := rows.Scan(&sub.ID, &sub.TenantID, &sub.UserID, &sub.Endpoint, &sub.P256dh, &sub.Auth, &createdAt); err != nil {
			return nil, err
		}
		sub.CreatedAt = createdAt.Time
		result = append(result, sub)
	}
	return result, rows.Err()
}

func (s *SQLitePushSubscriptionStore) DeleteByEndpoint(ctx context.Context, endpoint string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM push_subscriptions WHERE endpoint = ?`, endpoint)
	return err
}
