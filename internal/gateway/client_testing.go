package gateway

import (
	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/permissions"
)

// NewTestClient returns a minimally-wired Client for unit tests in other
// packages. Role + tenant are set directly because the underlying fields are
// unexported. SendResponse is safe because the send channel is nil — the
// writer hits the default branch of the select and drops the frame silently.
//
// Not for production use. Any non-test caller should use NewClient instead.
func NewTestClient(role permissions.Role, tenantID uuid.UUID, userID string) *Client {
	return &Client{
		id:            uuid.NewString(),
		authenticated: true,
		role:          role,
		userID:        userID,
		tenantID:      tenantID,
	}
}

// NewTestClientWithSend is NewTestClient + an observable response channel.
// Returns the client and the channel handler tests read raw frames from
// to assert response shape. Use this when the handler is read-only
// (no store side-effects to observe) — workflow.runState is the
// motivating case. Buffer size 16 is comfortably bigger than any
// single handler invocation emits.
//
// Not for production use.
func NewTestClientWithSend(role permissions.Role, tenantID uuid.UUID, userID string) (*Client, <-chan []byte) {
	send := make(chan []byte, 16)
	c := &Client{
		id:            uuid.NewString(),
		authenticated: true,
		role:          role,
		userID:        userID,
		tenantID:      tenantID,
		send:          send,
	}
	return c, send
}
