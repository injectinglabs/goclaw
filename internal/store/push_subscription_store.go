package store

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// PushSubscription is a single browser Web Push subscription endpoint for a user.
type PushSubscription struct {
	ID        uuid.UUID `json:"id" db:"id"`
	TenantID  uuid.UUID `json:"tenant_id" db:"tenant_id"`
	UserID    string    `json:"user_id" db:"user_id"`
	Endpoint  string    `json:"endpoint" db:"endpoint"`
	P256dh    string    `json:"p256dh" db:"p256dh"`
	Auth      string    `json:"auth" db:"auth"`
	CreatedAt time.Time `json:"created_at" db:"created_at"`
}

// PushSubscriptionStore manages per-user Web Push subscriptions.
type PushSubscriptionStore interface {
	// Upsert stores (or refreshes) a subscription, keyed by endpoint.
	Upsert(ctx context.Context, sub *PushSubscription) error
	// ListByUser returns all subscriptions for a user.
	ListByUser(ctx context.Context, userID string) ([]PushSubscription, error)
	// DeleteByEndpoint removes a subscription by its endpoint URL.
	DeleteByEndpoint(ctx context.Context, endpoint string) error
}
