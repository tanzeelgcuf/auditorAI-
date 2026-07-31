-- Per-tenant data encryption keys (doc 05 §5). One active key per firm at a time.
CREATE TABLE data_encryption_keys (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    firm_id UUID NOT NULL REFERENCES firms(id),
    key_ref TEXT NOT NULL,
    activated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'rotating', 'retired'))
);

CREATE INDEX idx_data_encryption_keys_firm ON data_encryption_keys(firm_id, status);

ALTER TABLE data_encryption_keys ENABLE ROW LEVEL SECURITY;
CREATE POLICY dek_firm_isolation ON data_encryption_keys
    USING (firm_id = current_setting('app.current_firm')::uuid);
