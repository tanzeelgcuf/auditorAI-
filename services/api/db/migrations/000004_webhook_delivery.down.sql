ALTER TABLE webhook_subscriptions DROP COLUMN IF EXISTS consecutive_failures;
ALTER TABLE webhook_subscriptions DROP COLUMN IF EXISTS enabled;
