-- Webhook delivery hardening (doc 07 §7): enable flag + failure tracking.
ALTER TABLE webhook_subscriptions ADD COLUMN enabled BOOLEAN NOT NULL DEFAULT true;
ALTER TABLE webhook_subscriptions ADD COLUMN consecutive_failures INTEGER NOT NULL DEFAULT 0;
