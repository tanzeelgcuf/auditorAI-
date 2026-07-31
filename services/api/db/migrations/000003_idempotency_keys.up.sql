-- Idempotency cache for uploads/report generation (doc 06 §4). TTL 24h.
CREATE TABLE idempotency_keys (
    key_hash TEXT PRIMARY KEY,
    user_id UUID NOT NULL,
    response_status INTEGER NOT NULL,
    response_body JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_idempotency_keys_created ON idempotency_keys (created_at);
