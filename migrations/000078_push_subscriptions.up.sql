-- 000078_push_subscriptions.up.sql
--
-- Web Push subscriptions: one row per browser push-subscription endpoint,
-- keyed by the endpoint URL (unique). Lets the gateway fan out a Web Push
-- notification to all of a user's subscribed browsers when a new-mail event
-- arrives (see internal/http/push_handler.go SendToUser).

BEGIN;

CREATE TABLE IF NOT EXISTS push_subscriptions (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    user_id    text NOT NULL,
    endpoint   text NOT NULL UNIQUE,
    p256dh     text NOT NULL,
    auth       text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_push_subscriptions_user
  ON push_subscriptions (user_id);

COMMIT;
