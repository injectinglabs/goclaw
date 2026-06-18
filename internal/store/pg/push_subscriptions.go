package pg

import (
	"context"
	"database/sql"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// PGPushSubscriptionStore implements store.PushSubscriptionStore backed by Postgres.
type PGPushSubscriptionStore struct {
	db *sql.DB
}

// NewPGPushSubscriptionStore creates a new PGPushSubscriptionStore.
func NewPGPushSubscriptionStore(db *sql.DB) *PGPushSubscriptionStore {
	return &PGPushSubscriptionStore{db: db}
}

func (s *PGPushSubscriptionStore) Upsert(ctx context.Context, sub *store.PushSubscription) error {
	tenantID := sub.TenantID
	if tenantID == uuid.Nil {
		tenantID = store.TenantIDFromContext(ctx)
	}
	if tenantID == uuid.Nil {
		tenantID = store.MasterTenantID
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO push_subscriptions (tenant_id, user_id, endpoint, p256dh, auth)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (endpoint) DO UPDATE SET
		   tenant_id = EXCLUDED.tenant_id,
		   user_id   = EXCLUDED.user_id,
		   p256dh    = EXCLUDED.p256dh,
		   auth      = EXCLUDED.auth`,
		tenantID, sub.UserID, sub.Endpoint, sub.P256dh, sub.Auth,
	)
	return err
}

func (s *PGPushSubscriptionStore) ListByUser(ctx context.Context, userID string) ([]store.PushSubscription, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, tenant_id, user_id, endpoint, p256dh, auth, created_at
		 FROM push_subscriptions WHERE user_id = $1`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []store.PushSubscription
	for rows.Next() {
		var sub store.PushSubscription
		if err := rows.Scan(&sub.ID, &sub.TenantID, &sub.UserID, &sub.Endpoint, &sub.P256dh, &sub.Auth, &sub.CreatedAt); err != nil {
			return nil, err
		}
		result = append(result, sub)
	}
	return result, rows.Err()
}

func (s *PGPushSubscriptionStore) DeleteByEndpoint(ctx context.Context, endpoint string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM push_subscriptions WHERE endpoint = $1`, endpoint)
	return err
}
