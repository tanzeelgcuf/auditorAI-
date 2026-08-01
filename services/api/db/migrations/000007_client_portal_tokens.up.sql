-- Client portal invite tokens (doc 07 §5).
ALTER TABLE client_portal_users ADD COLUMN invite_token TEXT;
ALTER TABLE client_portal_users ADD COLUMN invite_expires TIMESTAMPTZ;
