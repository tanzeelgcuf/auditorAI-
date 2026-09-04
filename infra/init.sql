-- AI Auditor v1 — Database Initialization (DDL from docs 05-10)
--
-- THIS FILE IS THE ENTIRE APPLIED SCHEMA. It is the only DDL that any
-- environment runs: infra/docker-compose*.yml mount it into
-- /docker-entrypoint-initdb.d/, and .github/workflows/ci.yml loads it with
-- `psql -v ON_ERROR_STOP=1 -f infra/init.sql`. There is no migration runner.
-- services/api/db/migrations/ used to hold 8 .up.sql files that NOTHING ever
-- applied — the only reference to that directory in the whole repo was a line
-- of prose in .claude/agents/backend-agent.md. Everything they created is
-- folded in below and the directory is deleted. Anything added here must also
-- be reflected in scripts/check_schema_drift.py's expectations by construction
-- (it parses this file), and services/api/sqlc.yaml validates against it.
--
-- ===== EXTENSIONS =====
-- BOTH PREVIOUS LINES WERE DELETED, and the second one was fatal:
--
--   CREATE EXTENSION IF NOT EXISTS "pgvector";
--
-- There is no extension named "pgvector". pgvector is the name of the PROJECT;
-- the extension it installs is named "vector" (its control file is
-- vector.control), so this statement cannot succeed on any PostgreSQL server —
-- IF NOT EXISTS does not help, because that only suppresses the error when the
-- extension is ALREADY INSTALLED under the name given. Separately, the official
-- postgres:16 image used by both compose files and by CI does not ship pgvector
-- at all (that needs pgvector/pgvector:pg16 or the postgresql-16-pgvector
-- package).
--
-- Because every runner of this file uses ON_ERROR_STOP=1, that one statement
-- aborted the whole script — so NO TABLE IN THIS FILE WAS EVER CREATED. That is
-- the true reason the 6 "missing" tables looked missing; in fact all 32 were.
-- Nothing in this repo uses a vector column, an embedding column, or any
-- pgvector operator (<->, <=>, ivfflat, hnsw): the vector store is qdrant, via
-- qdrant-client in services/agent-runtime/requirements.txt.
--
-- "uuid-ossp" is also gone: this file calls gen_random_uuid() 22 times and
-- uuid_generate_v4() zero times, and gen_random_uuid() has been in core
-- PostgreSQL since 13. No extension is required by this schema.

-- ===== TENANCY =====
CREATE TABLE firms (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name TEXT NOT NULL,
    stripe_customer_id TEXT,
    -- Subscription state, synced from Stripe by internal/billing.HandleStripeWebhook.
    --
    -- Added 2026-09-04. Before this the only billing column was stripe_customer_id,
    -- which was written in one place and read nowhere, and the
    -- customer.subscription.updated/deleted branch of the webhook was a bare
    -- slog.Info — so the application had no way to answer "is this firm's
    -- subscription active?" and a cancellation was logged and discarded.
    --
    -- All nullable: a firm exists before it subscribes. subscription_status holds
    -- Stripe's own status string verbatim (active, trialing, past_due, canceled,
    -- incomplete, ...) rather than a re-encoded local enum, so a status Stripe adds
    -- later is stored rather than rejected. No CHECK constraint for the same reason.
    subscription_status TEXT,
    -- starter | growth | scale, mapped from the Stripe price ID by
    -- billing.tierForPrice. NULL when the price is not one of the three known
    -- constants, which means the Stripe catalogue and the code have drifted.
    subscription_tier TEXT,
    subscription_current_period_end TIMESTAMPTZ,
    -- True when the firm has cancelled but is paid through the period end. Access
    -- decisions need both this and subscription_current_period_end: "cancelled" is
    -- not the same as "no longer entitled".
    subscription_cancel_at_period_end BOOLEAN NOT NULL DEFAULT false,
    -- Watermark: the `created` timestamp of the last Stripe event applied to the
    -- four columns above. Stripe does not guarantee event ordering, so the webhook
    -- compares against this and drops events older than what is already applied.
    -- Without it, a stale "active" delivered after a "canceled" would win.
    subscription_event_at TIMESTAMPTZ,
    logo_storage_key TEXT,
    brand_primary_color TEXT DEFAULT '#0F172A',
    report_footer_text TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The webhook looks a firm up by Stripe customer, because subscription events
-- carry a customer id and not the firm_id metadata that only
-- checkout.session.completed echoes back. Unique, not merely indexed: two firms
-- sharing one Stripe customer would make that lookup ambiguous and silently apply
-- one firm's subscription state to the other.
CREATE UNIQUE INDEX idx_firms_stripe_customer ON firms(stripe_customer_id)
    WHERE stripe_customer_id IS NOT NULL;

CREATE TABLE client_books (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    firm_id UUID NOT NULL REFERENCES firms(id),
    client_name TEXT NOT NULL,
    base_currency TEXT NOT NULL DEFAULT 'USD',
    reconciliation_tolerance_cents INTEGER NOT NULL DEFAULT 1,
    tolerance_mode TEXT NOT NULL DEFAULT 'fixed' CHECK (tolerance_mode IN ('fixed', 'percentage', 'greater_of')),
    tolerance_percentage NUMERIC(5,4),
    fiscal_year_start_month INTEGER NOT NULL DEFAULT 1 CHECK (fiscal_year_start_month BETWEEN 1 AND 12),
    auto_link_confidence_threshold NUMERIC(4,3) NOT NULL DEFAULT 0.85,
    review_confidence_floor NUMERIC(4,3) NOT NULL DEFAULT 0.50,
    require_separate_reviewer BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE users (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    firm_id UUID NOT NULL REFERENCES firms(id),
    email TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    role TEXT NOT NULL CHECK (role IN ('firm_admin', 'staff')),
    -- Second factor. `totp_secret` is the LIVE secret and is the single flag that
    -- decides whether login demands a code (auth.CheckSecondFactor treats NULL as
    -- "not enrolled"). `totp_pending_secret` holds a secret generated by
    -- /v1/totp/enable that has NOT yet been proven by the enrolling device; it is
    -- promoted to totp_secret only by /v1/totp/verify. Keeping them in separate
    -- columns is what makes enrollment a real ceremony — before 2026-09-04 the
    -- verify handler validated the code against a secret supplied in the same
    -- request body, which any caller could satisfy with a secret it generated
    -- itself.
    -- totp_last_code / totp_last_used_at are single-use replay memory: a code
    -- accepted once cannot be accepted again while it remains inside
    -- totp.Validate's +/-1 step skew window.
    totp_secret TEXT,
    totp_pending_secret TEXT,
    totp_enabled_at TIMESTAMPTZ,
    totp_last_code TEXT,
    totp_last_used_at TIMESTAMPTZ,
    email_verified BOOLEAN NOT NULL DEFAULT false,
    email_verification_token TEXT,
    email_verification_expires TIMESTAMPTZ,
    password_reset_token TEXT,
    password_reset_expires TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE user_book_assignments (
    user_id UUID NOT NULL REFERENCES users(id),
    client_book_id UUID NOT NULL REFERENCES client_books(id),
    PRIMARY KEY (user_id, client_book_id)
);

-- ===== DOCUMENTS & EXTRACTION =====
CREATE TABLE source_documents (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    client_book_id UUID NOT NULL REFERENCES client_books(id),
    filename TEXT NOT NULL,
    doc_type TEXT NOT NULL CHECK (doc_type IN ('invoice', 'bank_statement', 'gl_export')),
    storage_key TEXT NOT NULL,
    content_hash TEXT NOT NULL,
    page_count INTEGER,
    stated_opening_balance_cents BIGINT,
    stated_closing_balance_cents BIGINT,
    uploaded_by UUID NOT NULL REFERENCES users(id),
    uploaded_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    ocr_status TEXT NOT NULL DEFAULT 'pending' CHECK (ocr_status IN ('pending','processing','done','failed')),
    supersedes_document_id UUID REFERENCES source_documents(id),
    deleted_at TIMESTAMPTZ,
    retention_locked_until DATE
);
CREATE UNIQUE INDEX idx_unique_doc_per_book ON source_documents (client_book_id, content_hash) WHERE deleted_at IS NULL;

CREATE TABLE extracted_entities (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    client_book_id UUID NOT NULL REFERENCES client_books(id),
    source_document_id UUID NOT NULL REFERENCES source_documents(id),
    entity_type TEXT NOT NULL CHECK (entity_type IN ('invoice_line_item','bank_transaction','gl_entry')),
    entity_subtype TEXT CHECK (entity_subtype IN ('standard','credit_note','refund','void')),
    amount_cents BIGINT NOT NULL,
    currency TEXT NOT NULL DEFAULT 'USD',
    fx_rate_to_base NUMERIC(12,6),
    amount_cents_base BIGINT,
    debit_or_credit TEXT CHECK (debit_or_credit IN ('debit','credit')),
    transaction_date DATE,
    counterparty TEXT,
    description TEXT,
    gl_account_code TEXT,
    page_number INTEGER NOT NULL,
    bbox JSONB NOT NULL,
    extraction_confidence NUMERIC(4,3) NOT NULL,
    source_format TEXT NOT NULL DEFAULT 'ocr' CHECK (source_format IN ('ocr', 'structured')),
    transaction_ref TEXT,
    -- Folded from migration 000008 (doc 11 §1, human override / manual entry).
    -- humanoverride.go:99-102 INSERTs created_by/manually_created_by/
    -- corrects_entity_id and :116 UPDATEs status, so POST
    -- /v1/books/{bookId}/entities/manual could not work without these four.
    created_by TEXT NOT NULL DEFAULT 'system' CHECK (created_by IN ('system', 'manual')),
    manually_created_by UUID REFERENCES users(id),
    corrects_entity_id UUID REFERENCES extracted_entities(id),
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'superseded_by_manual')),
    extracted_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_extracted_entities_ref ON extracted_entities (client_book_id, transaction_ref);

-- ===== CROSS-LINKING & REVIEW =====
CREATE TABLE reconciliation_groups (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    client_book_id UUID NOT NULL REFERENCES client_books(id),
    link_confidence NUMERIC(4,3) NOT NULL,
    -- 'superseded' folded from migration 000008 §2. humanoverride.go:186 (split)
    -- and :274 (merge) both write it; without it those two writes violated this
    -- CHECK and the endpoints 500'd.
    status TEXT NOT NULL DEFAULT 'auto_linked' CHECK (status IN ('auto_linked','needs_review','confirmed','rejected','superseded')),
    -- Folded from migration 000010 (doc 12 §2): AP vs AR reconciled separately.
    -- mcp.go:234 INSERTs this on the agent's create_entity_link write path.
    group_scope TEXT NOT NULL DEFAULT 'ap' CHECK (group_scope IN ('ap', 'ar', 'other')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_reconciliation_groups_scope ON reconciliation_groups (client_book_id, group_scope);

CREATE TABLE reconciliation_group_members (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    reconciliation_group_id UUID NOT NULL REFERENCES reconciliation_groups(id),
    extracted_entity_id UUID NOT NULL REFERENCES extracted_entities(id),
    role TEXT NOT NULL CHECK (role IN ('invoice','bank','gl')),
    UNIQUE (reconciliation_group_id, extracted_entity_id)
);

-- ===== FINDINGS (output of services/verification ONLY) =====
CREATE TABLE audit_findings (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    client_book_id UUID NOT NULL REFERENCES client_books(id),
    reconciliation_group_id UUID NOT NULL REFERENCES reconciliation_groups(id),
    rule_id TEXT NOT NULL,
    rule_version TEXT NOT NULL,
    calculated_variance_cents BIGINT NOT NULL,
    tolerance_cents INTEGER NOT NULL,
    exceeds_tolerance BOOLEAN NOT NULL,
    calculation_formula TEXT NOT NULL,
    severity TEXT NOT NULL CHECK (severity IN ('info','low','medium','high')),
    status TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open','acknowledged','resolved')),
    prepared_by UUID REFERENCES users(id),
    reviewed_by UUID REFERENCES users(id),
    reviewed_at TIMESTAMPTZ,
    due_date DATE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE finding_comments (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    audit_finding_id UUID NOT NULL REFERENCES audit_findings(id),
    user_id UUID NOT NULL REFERENCES users(id),
    comment TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE finding_attachments (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    audit_finding_id UUID NOT NULL REFERENCES audit_findings(id),
    uploaded_by UUID NOT NULL REFERENCES users(id),
    storage_key TEXT NOT NULL,
    filename TEXT NOT NULL,
    uploaded_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ===== REPORTS =====
CREATE TABLE audit_reports (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    client_book_id UUID NOT NULL REFERENCES client_books(id),
    period_start DATE NOT NULL,
    period_end DATE NOT NULL,
    generated_by UUID NOT NULL REFERENCES users(id),
    generated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    pdf_storage_key TEXT,
    finding_ids UUID[] NOT NULL DEFAULT '{}'
);

-- ===== RECONCILIATION PERIODS & TRIAL BALANCE =====
CREATE TABLE reconciliation_periods (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    client_book_id UUID NOT NULL REFERENCES client_books(id),
    period_start DATE NOT NULL,
    period_end DATE NOT NULL,
    opening_unreconciled_entity_ids UUID[] NOT NULL DEFAULT '{}',
    trial_balance_debits_cents BIGINT,
    trial_balance_credits_cents BIGINT,
    trial_balance_is_balanced BOOLEAN,
    status TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'pending_close', 'closed', 'reopened')),
    closed_by UUID REFERENCES users(id),
    closed_at TIMESTAMPTZ,
    UNIQUE (client_book_id, period_start, period_end)
);

CREATE TABLE period_reopen_log (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    reconciliation_period_id UUID NOT NULL REFERENCES reconciliation_periods(id),
    reopened_by UUID NOT NULL REFERENCES users(id),
    reason TEXT NOT NULL,
    reopened_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ===== CHART OF ACCOUNTS & MAPPINGS =====
CREATE TABLE chart_of_accounts (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    client_book_id UUID NOT NULL REFERENCES client_books(id),
    account_code TEXT NOT NULL,
    account_name TEXT NOT NULL,
    account_type TEXT NOT NULL CHECK (account_type IN ('asset','liability','equity','revenue','expense')),
    is_reconcilable BOOLEAN NOT NULL DEFAULT true,
    UNIQUE (client_book_id, account_code)
);

CREATE TABLE csv_column_mappings (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    client_book_id UUID NOT NULL REFERENCES client_books(id),
    source_system TEXT,
    column_map JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE counterparty_aliases (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    client_book_id UUID NOT NULL REFERENCES client_books(id),
    canonical_name TEXT NOT NULL,
    alias TEXT NOT NULL,
    confirmed_by UUID REFERENCES users(id),
    UNIQUE (client_book_id, alias)
);

-- ===== VENDOR SPEND BASELINES (anomaly detection) =====
CREATE TABLE vendor_spend_baselines (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    client_book_id UUID NOT NULL REFERENCES client_books(id),
    counterparty_canonical_name TEXT NOT NULL,
    trailing_avg_cents BIGINT NOT NULL,
    trailing_stddev_cents BIGINT,
    computed_through_period_id UUID REFERENCES reconciliation_periods(id),
    UNIQUE (client_book_id, counterparty_canonical_name)
);

-- ===== DOCUMENT REQUESTS =====
CREATE TABLE document_requests (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    client_book_id UUID NOT NULL REFERENCES client_books(id),
    reconciliation_period_id UUID REFERENCES reconciliation_periods(id),
    requested_doc_type TEXT NOT NULL,
    description TEXT,
    requested_by UUID NOT NULL REFERENCES users(id),
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','fulfilled','waived')),
    fulfilled_by_document_id UUID REFERENCES source_documents(id),
    requested_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    reminder_sent_count INTEGER NOT NULL DEFAULT 0
);

-- ===== API KEYS & WEBHOOKS =====
CREATE TABLE api_keys (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    firm_id UUID NOT NULL REFERENCES firms(id),
    key_hash TEXT NOT NULL,
    created_by UUID NOT NULL REFERENCES users(id),
    last_used_at TIMESTAMPTZ,
    -- created_at was MISSING and is not in any migration either — this is a
    -- twelfth drift item, found by scripts/check_schema_drift.py rather than by
    -- reading: settings.go:749 ends `ORDER BY created_at DESC`, so
    -- GET /v1/admin/api-keys errored with `column "created_at" does not exist`
    -- on every call. Every other table here carries this column.
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at TIMESTAMPTZ
);

CREATE TABLE idempotency_keys (
    key_hash TEXT PRIMARY KEY,
    user_id UUID NOT NULL,
    response_status INTEGER NOT NULL,
    response_body JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_idempotency_keys_created ON idempotency_keys (created_at);

CREATE TABLE webhook_subscriptions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    firm_id UUID NOT NULL REFERENCES firms(id),
    target_url TEXT NOT NULL,
    event_types TEXT[] NOT NULL,
    signing_secret TEXT NOT NULL,
    -- Folded from migration 000004. These two are on the REPORT GENERATION path,
    -- not an optional one: webhooks.go:38-58 SELECTs `enabled` and :140-152
    -- UPDATEs `consecutive_failures`, and the notifier is wired live at
    -- main.go:173,175 and called by HandleGenerateReport
    -- (POST /v1/books/{bookId}/reports) — the product's primary output.
    enabled BOOLEAN NOT NULL DEFAULT true,
    consecutive_failures INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ===== ACCESS LOG =====
CREATE TABLE access_log (
    id BIGSERIAL PRIMARY KEY,
    user_id UUID NOT NULL REFERENCES users(id),
    client_book_id UUID REFERENCES client_books(id),
    action TEXT NOT NULL,
    resource_id UUID,
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ===== NOTIFICATION PREFERENCES =====
CREATE TABLE notification_preferences (
    user_id UUID PRIMARY KEY REFERENCES users(id),
    high_severity_immediate BOOLEAN NOT NULL DEFAULT true,
    daily_digest BOOLEAN NOT NULL DEFAULT true,
    digest_send_hour INTEGER NOT NULL DEFAULT 8
);

-- ===== CLIENT PORTAL USERS =====
CREATE TABLE client_portal_users (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    client_book_id UUID NOT NULL REFERENCES client_books(id),
    email TEXT NOT NULL,
    invited_by UUID NOT NULL REFERENCES users(id),
    -- Folded from migration 000007. portal.go:64 SELECTs both on
    -- POST /v1/portal/login, so portal login could not work without them.
    invite_token TEXT,
    invite_expires TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ===== COA TEMPLATES =====
CREATE TABLE coa_templates (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    template_name TEXT NOT NULL,
    industry TEXT,
    accounts JSONB NOT NULL
);

-- ===== FOLDED FROM services/api/db/migrations/ (never-applied migrations) =====
-- Everything below this line existed only in .up.sql files that no runner ever
-- executed. Each object is annotated with the code that reads or writes it and
-- the HTTP route that reaches it, so a future reader can tell live schema from
-- speculative schema.

-- From 000002. Per-tenant data encryption keys (doc 05 §5).
-- rotate_keys.go:50 SELECTs and :60 INSERTs -> POST /v1/tenant/rotate-keys.
-- ALSO: middleware/security_test.go:88 TRUNCATEs this table in test setup, so
-- its absence made the entire security/RLS suite die before its first
-- assertion — a schema bug masquerading as passing tenant-isolation coverage.
CREATE TABLE data_encryption_keys (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    firm_id UUID NOT NULL REFERENCES firms(id),
    key_ref TEXT NOT NULL,
    activated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'rotating', 'retired'))
);
CREATE INDEX idx_data_encryption_keys_firm ON data_encryption_keys(firm_id, status);

-- From 000005. Mobile push targets (doc 03 §3.10 / doc 07 §8).
-- push.go:78 INSERTs -> POST /v1/push/register, which apps/mobile/src/push.ts:15
-- already calls; push.go:102 SELECTs for delivery.
CREATE TABLE device_tokens (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    firm_id UUID NOT NULL REFERENCES firms(id),
    user_id UUID NOT NULL REFERENCES users(id),
    token TEXT NOT NULL UNIQUE,
    platform TEXT NOT NULL CHECK (platform IN ('ios','android')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- From 000008 §3. Config audit trail: every tolerance/threshold change on a book.
-- tenant.go:302-318 INSERTs on the book-settings update path and SELECTs for
-- GET /v1/books/{bookId}/config-history.
CREATE TABLE config_change_log (
    id BIGSERIAL PRIMARY KEY,
    client_book_id UUID NOT NULL REFERENCES client_books(id),
    changed_by UUID NOT NULL REFERENCES users(id),
    field_name TEXT NOT NULL,
    old_value TEXT,
    new_value TEXT,
    changed_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- From 000008 §4. Business-day-aware date matching.
-- NOTE, and this is a real gap rather than an oversight in the fold: `grep -rn
-- bank_holidays` finds NO reader anywhere in services/ or apps/. The table is
-- carried over so the schema is complete against doc 11, but the business-day
-- matching feature it exists for is not implemented. client_book_id is
-- deliberately nullable = a global (all-books) holiday.
CREATE TABLE bank_holidays (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    client_book_id UUID,
    holiday_date DATE NOT NULL,
    description TEXT
);

-- From 000008 §6. Custom tags. tags -> GET/POST /v1/tags;
-- entity_tags -> POST /v1/entities/tag.
CREATE TABLE tags (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    firm_id UUID NOT NULL REFERENCES firms(id),
    label TEXT NOT NULL,
    color TEXT,
    UNIQUE (firm_id, label)
);

CREATE TABLE entity_tags (
    extracted_entity_id UUID NOT NULL REFERENCES extracted_entities(id),
    tag_id UUID NOT NULL REFERENCES tags(id),
    tagged_by UUID NOT NULL REFERENCES users(id),
    PRIMARY KEY (extracted_entity_id, tag_id)
);
CREATE INDEX idx_entity_tags_entity ON entity_tags (extracted_entity_id);

-- From 000008 §5. Automation-rate view -> GET /v1/books/{bookId}/automation-rate.
--
-- RLS CAVEAT, deliberately recorded here: a view is NOT subject to the RLS of
-- its base table on behalf of the querying user — it runs with the privileges of
-- the VIEW OWNER unless declared WITH (security_invoker = true). Postgres 15+
-- supports security_invoker, and this schema targets postgres:16, so it is set.
-- Without it this view would be a clean read-around of
-- groups_book_isolation, returning every firm's counts to any caller.
CREATE VIEW book_automation_rate WITH (security_invoker = true) AS
SELECT
    client_book_id,
    COUNT(*) FILTER (WHERE status = 'auto_linked') AS auto_linked_count,
    COUNT(*) FILTER (WHERE status = 'needs_review') AS needs_review_count,
    COUNT(*) FILTER (WHERE status = 'confirmed') AS confirmed_count,
    COUNT(*) AS total_count
FROM reconciliation_groups
GROUP BY client_book_id;

-- ===== ROW LEVEL SECURITY =====
-- Firms
ALTER TABLE firms ENABLE ROW LEVEL SECURITY;
CREATE POLICY firm_self_only ON firms USING (id = current_setting('app.current_firm')::uuid);

-- Client books
ALTER TABLE client_books ENABLE ROW LEVEL SECURITY;
CREATE POLICY client_books_firm_isolation ON client_books USING (firm_id = current_setting('app.current_firm')::uuid);

-- Users
ALTER TABLE users ENABLE ROW LEVEL SECURITY;
CREATE POLICY users_firm_isolation ON users USING (firm_id = current_setting('app.current_firm')::uuid);

-- User book assignments
ALTER TABLE user_book_assignments ENABLE ROW LEVEL SECURITY;
CREATE POLICY assignments_own_firm_only ON user_book_assignments
    USING (client_book_id IN (
        SELECT id FROM client_books WHERE firm_id = current_setting('app.current_firm')::uuid
    ));

-- Source documents
ALTER TABLE source_documents ENABLE ROW LEVEL SECURITY;
CREATE POLICY documents_book_isolation ON source_documents
    USING (client_book_id = ANY(string_to_array(current_setting('app.assigned_books'), ',')::uuid[]));

-- Extracted entities
ALTER TABLE extracted_entities ENABLE ROW LEVEL SECURITY;
CREATE POLICY entities_book_isolation ON extracted_entities
    USING (client_book_id = ANY(string_to_array(current_setting('app.assigned_books'), ',')::uuid[]));

-- Reconciliation groups
ALTER TABLE reconciliation_groups ENABLE ROW LEVEL SECURITY;
CREATE POLICY groups_book_isolation ON reconciliation_groups
    USING (client_book_id = ANY(string_to_array(current_setting('app.assigned_books'), ',')::uuid[]));

-- Reconciliation group members
ALTER TABLE reconciliation_group_members ENABLE ROW LEVEL SECURITY;
CREATE POLICY members_book_isolation ON reconciliation_group_members
    USING (reconciliation_group_id IN (
        SELECT id FROM reconciliation_groups
        WHERE client_book_id = ANY(string_to_array(current_setting('app.assigned_books'), ',')::uuid[])
    ));

-- Audit findings
ALTER TABLE audit_findings ENABLE ROW LEVEL SECURITY;
CREATE POLICY findings_book_isolation ON audit_findings
    USING (client_book_id = ANY(string_to_array(current_setting('app.assigned_books'), ',')::uuid[]));

-- Finding comments
ALTER TABLE finding_comments ENABLE ROW LEVEL SECURITY;
CREATE POLICY comments_via_finding_book_isolation ON finding_comments
    USING (audit_finding_id IN (
        SELECT id FROM audit_findings
        WHERE client_book_id = ANY(string_to_array(current_setting('app.assigned_books'), ',')::uuid[])
    ));

-- Finding attachments
ALTER TABLE finding_attachments ENABLE ROW LEVEL SECURITY;
CREATE POLICY attachments_via_finding_book_isolation ON finding_attachments
    USING (audit_finding_id IN (
        SELECT id FROM audit_findings
        WHERE client_book_id = ANY(string_to_array(current_setting('app.assigned_books'), ',')::uuid[])
    ));

-- Audit reports
ALTER TABLE audit_reports ENABLE ROW LEVEL SECURITY;
CREATE POLICY reports_book_isolation ON audit_reports
    USING (client_book_id = ANY(string_to_array(current_setting('app.assigned_books'), ',')::uuid[]));

-- Reconciliation periods
ALTER TABLE reconciliation_periods ENABLE ROW LEVEL SECURITY;
CREATE POLICY periods_book_isolation ON reconciliation_periods
    USING (client_book_id = ANY(string_to_array(current_setting('app.assigned_books'), ',')::uuid[]));

-- Period reopen log
ALTER TABLE period_reopen_log ENABLE ROW LEVEL SECURITY;
CREATE POLICY reopen_log_book_isolation ON period_reopen_log
    USING (reconciliation_period_id IN (
        SELECT id FROM reconciliation_periods
        WHERE client_book_id = ANY(string_to_array(current_setting('app.assigned_books'), ',')::uuid[])
    ));

-- Chart of accounts
ALTER TABLE chart_of_accounts ENABLE ROW LEVEL SECURITY;
CREATE POLICY coa_book_isolation ON chart_of_accounts
    USING (client_book_id = ANY(string_to_array(current_setting('app.assigned_books'), ',')::uuid[]));

-- CSV column mappings
ALTER TABLE csv_column_mappings ENABLE ROW LEVEL SECURITY;
CREATE POLICY csv_mappings_book_isolation ON csv_column_mappings
    USING (client_book_id = ANY(string_to_array(current_setting('app.assigned_books'), ',')::uuid[]));

-- Counterparty aliases
ALTER TABLE counterparty_aliases ENABLE ROW LEVEL SECURITY;
CREATE POLICY aliases_book_isolation ON counterparty_aliases
    USING (client_book_id = ANY(string_to_array(current_setting('app.assigned_books'), ',')::uuid[]));

-- Vendor spend baselines
ALTER TABLE vendor_spend_baselines ENABLE ROW LEVEL SECURITY;
CREATE POLICY baselines_book_isolation ON vendor_spend_baselines
    USING (client_book_id = ANY(string_to_array(current_setting('app.assigned_books'), ',')::uuid[]));

-- Document requests
ALTER TABLE document_requests ENABLE ROW LEVEL SECURITY;
CREATE POLICY requests_book_isolation ON document_requests
    USING (client_book_id = ANY(string_to_array(current_setting('app.assigned_books'), ',')::uuid[]));

-- Access log
ALTER TABLE access_log ENABLE ROW LEVEL SECURITY;
CREATE POLICY access_log_own_firm_only ON access_log
    USING (client_book_id IS NULL OR client_book_id = ANY(string_to_array(current_setting('app.assigned_books'), ',')::uuid[]));

-- Notification preferences
ALTER TABLE notification_preferences ENABLE ROW LEVEL SECURITY;
CREATE POLICY prefs_own_firm_only ON notification_preferences
    USING (user_id IN (SELECT id FROM users WHERE firm_id = current_setting('app.current_firm')::uuid));

-- Client portal users
ALTER TABLE client_portal_users ENABLE ROW LEVEL SECURITY;
CREATE POLICY portal_users_book_isolation ON client_portal_users
    USING (client_book_id = ANY(string_to_array(current_setting('app.assigned_books'), ',')::uuid[]));

-- API keys
ALTER TABLE api_keys ENABLE ROW LEVEL SECURITY;
CREATE POLICY api_keys_firm_isolation ON api_keys
    USING (firm_id = current_setting('app.current_firm')::uuid);

-- Webhook subscriptions
ALTER TABLE webhook_subscriptions ENABLE ROW LEVEL SECURITY;
CREATE POLICY webhooks_firm_isolation ON webhook_subscriptions
    USING (firm_id = current_setting('app.current_firm')::uuid);

-- ----- RLS for the tables folded in from services/api/db/migrations/ -----
-- Migration 000002 shipped a policy for data_encryption_keys; the other five
-- tables it added had NONE, which means that even after the role split below
-- they would have stayed world-readable within the database. That gap is closed
-- here.

-- Data encryption keys (policy text carried over verbatim from 000002).
ALTER TABLE data_encryption_keys ENABLE ROW LEVEL SECURITY;
CREATE POLICY dek_firm_isolation ON data_encryption_keys
    USING (firm_id = current_setting('app.current_firm')::uuid);

-- Device tokens. firm-scoped: a push token is a user's device, and users are
-- firm-scoped.
ALTER TABLE device_tokens ENABLE ROW LEVEL SECURITY;
CREATE POLICY device_tokens_firm_isolation ON device_tokens
    USING (firm_id = current_setting('app.current_firm')::uuid);

-- Config change log. book-scoped, same shape as the other per-book audit trails.
ALTER TABLE config_change_log ENABLE ROW LEVEL SECURITY;
CREATE POLICY config_change_log_book_isolation ON config_change_log
    USING (client_book_id = ANY(string_to_array(current_setting('app.assigned_books'), ',')::uuid[]));

-- Bank holidays. client_book_id IS NULL means a GLOBAL holiday row, readable by
-- everyone by design; a non-null value restricts it to that book.
ALTER TABLE bank_holidays ENABLE ROW LEVEL SECURITY;
CREATE POLICY bank_holidays_book_or_global ON bank_holidays
    USING (client_book_id IS NULL OR client_book_id = ANY(string_to_array(current_setting('app.assigned_books'), ',')::uuid[]));

-- Tags are firm-level (UNIQUE (firm_id, label)), not book-level.
ALTER TABLE tags ENABLE ROW LEVEL SECURITY;
CREATE POLICY tags_firm_isolation ON tags
    USING (firm_id = current_setting('app.current_firm')::uuid);

-- Entity tags: reachable only through an entity in an assigned book. Written as
-- a subquery on extracted_entities rather than trusting entity_tags' own
-- columns, because it has none that carry tenancy.
ALTER TABLE entity_tags ENABLE ROW LEVEL SECURITY;
CREATE POLICY entity_tags_book_isolation ON entity_tags
    USING (extracted_entity_id IN (
        SELECT id FROM extracted_entities
        WHERE client_book_id = ANY(string_to_array(current_setting('app.assigned_books'), ',')::uuid[])
    ));

-- ----- DELIBERATELY NOT RLS-PROTECTED (documented, not overlooked) -----
-- coa_templates: global chart-of-accounts templates, no tenant column. Read by
--   templates.go:16,28 and settings.go:351,407.
-- idempotency_keys: keyed by key_hash = sha256(user_id || ':' || client key)
--   (middleware/idempotency.go:35), so a key from firm A cannot collide with
--   firm B's. It is also queried on the RAW pool (idempotency.go:41,102), which
--   has no tenant GUC set, so adding RLS here would break every retry-safe
--   request instead of protecting anything.

-- ===== INDEXES =====
CREATE INDEX idx_source_documents_book ON source_documents(client_book_id);
CREATE INDEX idx_extracted_entities_book ON extracted_entities(client_book_id);
CREATE INDEX idx_extracted_entities_doc ON extracted_entities(source_document_id);
CREATE INDEX idx_reconciliation_groups_book ON reconciliation_groups(client_book_id);
CREATE INDEX idx_audit_findings_book ON audit_findings(client_book_id);
CREATE INDEX idx_audit_findings_group ON audit_findings(reconciliation_group_id);
CREATE INDEX idx_audit_reports_book ON audit_reports(client_book_id);
CREATE INDEX idx_reconciliation_periods_book ON reconciliation_periods(client_book_id);
CREATE INDEX idx_chart_of_accounts_book ON chart_of_accounts(client_book_id);
CREATE INDEX idx_counterparty_aliases_book ON counterparty_aliases(client_book_id);
CREATE INDEX idx_document_requests_book ON document_requests(client_book_id);
CREATE INDEX idx_access_log_user ON access_log(user_id);
CREATE INDEX idx_access_log_book ON access_log(client_book_id);

-- ============================================================================
-- APPLICATION ROLES  —  this is what makes the 30 policies above do anything
-- ============================================================================
-- Every policy above was INERT as deployed, and not because of a typo:
--
--   * POSTGRES_USER=auditor is initdb's bootstrap role, i.e. a SUPERUSER, and
--     it owns every table here. Postgres exempts superusers from row security
--     unconditionally, and exempts table owners unless the table is set to
--     FORCE. The api connected with that role, so `ENABLE ROW LEVEL SECURITY`
--     on 24 tables changed nothing at all.
--   * There was no CREATE ROLE and no GRANT anywhere in this file, so no
--     non-exempt role existed to connect as.
--
-- Be precise about what FORCE does and does not fix: the ALTER ... FORCE
-- statements further down subject the TABLE OWNER to policies, but they do NOT
-- subject a SUPERUSER. So FORCE alone would still leave this wide open while the
-- api connects as `auditor`. The control that actually works is the role split
-- below; FORCE is defence-in-depth for the day ownership moves off a superuser.
--
-- TWO roles, because the code has two legitimately different access patterns
-- and pretending otherwise is what would break production:
--
--   auditor_app  — NOSUPERUSER, NOBYPASSRLS. The per-request path. Every query
--                  it runs is filtered by the policies above, which is only
--                  sound because middleware.RLSInjector sets app.current_firm
--                  and app.assigned_books on a DEDICATED connection per request
--                  (middleware.go:118-150).
--   auditor_sys  — NOSUPERUSER but BYPASSRLS. For the paths that are
--                  structurally cross-tenant and cannot be expressed as one
--                  firm id: (a) pre-auth identity lookups — signup INSERTs the
--                  firm row that would have to already exist for a policy to
--                  match (auth.go:270), login resolves firm_id FROM the email
--                  (auth.go:331), plus verify-email, forgot/reset-password and
--                  portal login (portal.go:63); (b) background workers that
--                  sweep all firms by design — notify.Run (notify.go:51,75),
--                  pipeline.Coordinator (coordinator.go:162,198,288) and
--                  verify_worker (verify_worker.go:113,161); (c)
--                  middleware/internal_auth.go:71,86, which must resolve
--                  book -> firm BEFORE it can set the GUC at :104.
--
-- BYPASSRLS rather than superuser is the point: auditor_sys can read across
-- tenants but cannot create objects, cannot read other databases, and holds only
-- the DML grants issued below.
-- Passwords come from the environment, never from this file. Both compose (via
-- the postgres service env) and CI must export APP_DB_PASSWORD and
-- SYS_DB_PASSWORD or this script stops here on purpose.
--
-- \getenv, not `printf '%s' "$APP_DB_PASSWORD"`. The backquote form makes psql
-- run a SHELL, and inside double quotes the shell still expands $(...) and
-- backticks — so a password containing either would have been executed as a
-- command during database init. \getenv (psql 14+, and the postgres:16 image
-- ships psql 16) reads the variable directly with no shell involved.
--
-- The two \set lines are not redundant: \getenv leaves the psql variable
-- UNCHANGED when the environment variable is absent, so without a defined
-- default, :'app_pw' would be emitted literally and fail with a syntax error
-- instead of the actionable message the DO block below raises.
\set app_pw ''
\set sys_pw ''
\getenv app_pw APP_DB_PASSWORD
\getenv sys_pw SYS_DB_PASSWORD
SELECT set_config('auditor.bootstrap_app_pw', :'app_pw', false);
SELECT set_config('auditor.bootstrap_sys_pw', :'sys_pw', false);

DO $bootstrap$
DECLARE
    app_pw text := current_setting('auditor.bootstrap_app_pw', true);
    sys_pw text := current_setting('auditor.bootstrap_sys_pw', true);
BEGIN
    IF app_pw IS NULL OR length(app_pw) < 16 THEN
        RAISE EXCEPTION 'APP_DB_PASSWORD is unset or shorter than 16 chars. '
            'Set it in .env (see .env.example) and re-create the postgres volume; '
            'the api connects as auditor_app and RLS depends on it.';
    END IF;
    IF sys_pw IS NULL OR length(sys_pw) < 16 THEN
        RAISE EXCEPTION 'SYS_DB_PASSWORD is unset or shorter than 16 chars. '
            'Set it in .env (see .env.example); auth and the background workers '
            'connect as auditor_sys.';
    END IF;
    IF app_pw = sys_pw THEN
        RAISE EXCEPTION 'APP_DB_PASSWORD and SYS_DB_PASSWORD must differ; they '
            'are the RLS-enforced and RLS-bypassing credentials respectively.';
    END IF;

    -- format(%L) quotes and escapes; the literal never appears in this file.
    EXECUTE format(
        'CREATE ROLE auditor_app LOGIN PASSWORD %L '
        'NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS NOINHERIT', app_pw);
    EXECUTE format(
        'CREATE ROLE auditor_sys LOGIN PASSWORD %L '
        'NOSUPERUSER NOCREATEDB NOCREATEROLE BYPASSRLS NOINHERIT', sys_pw);
END
$bootstrap$;

-- Scrub the passwords out of the session before anything else runs.
SELECT set_config('auditor.bootstrap_app_pw', '', false);
SELECT set_config('auditor.bootstrap_sys_pw', '', false);
-- ----- PRIVILEGES -----
-- Least privilege: DML only. No CREATE, no TRUNCATE, no ownership. Test suites
-- that need TRUNCATE (middleware/security_test.go:86) must connect as the owner
-- for setup and as auditor_app for the isolation assertions — that split is the
-- whole point of the suite.
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT USAGE ON SCHEMA public TO auditor_app, auditor_sys;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public
    TO auditor_app, auditor_sys;
-- BIGSERIAL columns (access_log.id, config_change_log.id, period_reopen_log.id)
-- need the sequence, or every INSERT into them fails with "permission denied for
-- sequence".
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO auditor_app, auditor_sys;

-- ----- FORCE ROW LEVEL SECURITY -----
-- Applies policies to the table owner too. As noted above this does NOT cover a
-- superuser, so it is not what fixes the inert-RLS bug; it exists so that the
-- owner cannot quietly become a bypass path later. Generated over pg_class
-- instead of 30 hand-written ALTERs so that it cannot drift out of step with the
-- ENABLE statements above — if a future table gets ENABLE and no FORCE, this
-- loop catches it.
DO $force_rls$
DECLARE
    t regclass;
    n integer := 0;
BEGIN
    FOR t IN
        SELECT c.oid::regclass
        FROM pg_class c
        JOIN pg_namespace ns ON ns.oid = c.relnamespace
        WHERE ns.nspname = 'public'
          AND c.relkind = 'r'
          AND c.relrowsecurity          -- ENABLE ROW LEVEL SECURITY is set
          AND NOT c.relforcerowsecurity -- ...but FORCE is not
        ORDER BY c.relname
    LOOP
        EXECUTE format('ALTER TABLE %s FORCE ROW LEVEL SECURITY', t);
        n := n + 1;
    END LOOP;
    RAISE NOTICE 'FORCE ROW LEVEL SECURITY applied to % table(s)', n;

    -- Fail the whole init if any table carries a policy but never got ENABLE —
    -- a policy on a non-RLS table is silently dead weight, and that class of
    -- mistake is exactly what left this schema unprotected in the first place.
    SELECT count(*) INTO n
    FROM (SELECT DISTINCT polrelid FROM pg_policy) p
    JOIN pg_class c ON c.oid = p.polrelid
    WHERE NOT c.relrowsecurity;
    IF n > 0 THEN
        RAISE EXCEPTION '% table(s) have policies but no ENABLE ROW LEVEL SECURITY', n;
    END IF;
END
$force_rls$;
-- ============================================================================
-- SEPARATE DATABASES FOR THE OBSERVABILITY SIDECARS
-- ============================================================================
-- langfuse and glitchtip previously pointed at THIS database and run their own
-- migrations as a superuser on startup. That is not merely untidy — it collides.
-- Langfuse v2 (Prisma/next-auth) creates `users` and `api_keys`; GlitchTip
-- (Django) creates `users` too. Both names already exist here, holding
-- authentication data. Best case their migration aborts; worst case a monitoring
-- sidecar ALTERs the table the application authenticates against.
--
-- CREATE DATABASE cannot run inside a transaction block. This file is executed by
-- psql WITHOUT --single-transaction (the postgres image's docker_process_sql, and
-- the CI step, both invoke `psql -v ON_ERROR_STOP=1 -f`), so each statement
-- autocommits and these are legal here. Do not add --single-transaction.
CREATE DATABASE langfuse OWNER auditor;
CREATE DATABASE glitchtip OWNER auditor;

-- ============================================================================
-- SELF-CHECK — fail the init rather than come up half-configured
-- ============================================================================
DO $selfcheck$
DECLARE
    missing text;
    n integer;
BEGIN
    -- 1. Both roles exist and have the intended bypass posture.
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'auditor_app'
                   AND NOT rolbypassrls AND NOT rolsuper) THEN
        RAISE EXCEPTION 'auditor_app missing, or is superuser/BYPASSRLS — RLS would be inert';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'auditor_sys'
                   AND rolbypassrls AND NOT rolsuper) THEN
        RAISE EXCEPTION 'auditor_sys missing, or lacks BYPASSRLS — auth and workers would 500';
    END IF;

    -- 2. Every RLS-enabled table actually has a policy AND has FORCE set.
    SELECT string_agg(c.relname, ', ' ORDER BY c.relname) INTO missing
    FROM pg_class c
    JOIN pg_namespace ns ON ns.oid = c.relnamespace
    WHERE ns.nspname = 'public' AND c.relkind = 'r' AND c.relrowsecurity
      AND (NOT c.relforcerowsecurity
           OR NOT EXISTS (SELECT 1 FROM pg_policy p WHERE p.polrelid = c.oid));
    IF missing IS NOT NULL THEN
        RAISE EXCEPTION 'RLS-enabled but unforced or policy-less: %', missing;
    END IF;

    -- 3. The two intentional exemptions are still the ONLY ones. Any new table
    --    that forgets RLS trips this instead of silently shipping unprotected.
    SELECT string_agg(c.relname, ', ' ORDER BY c.relname) INTO missing
    FROM pg_class c
    JOIN pg_namespace ns ON ns.oid = c.relnamespace
    WHERE ns.nspname = 'public' AND c.relkind = 'r' AND NOT c.relrowsecurity
      AND c.relname NOT IN ('coa_templates', 'idempotency_keys');
    IF missing IS NOT NULL THEN
        RAISE EXCEPTION 'table(s) without RLS and not on the documented exemption '
            'list: %. Add a policy, or add it to the list here and say why.', missing;
    END IF;

    -- 4. security_invoker on the view, or it reads around groups_book_isolation.
    IF NOT EXISTS (
        SELECT 1 FROM pg_class c
        JOIN pg_namespace ns ON ns.oid = c.relnamespace
        WHERE ns.nspname = 'public' AND c.relname = 'book_automation_rate'
          AND c.relkind = 'v' AND c.reloptions @> ARRAY['security_invoker=true']
    ) THEN
        RAISE EXCEPTION 'book_automation_rate is not security_invoker — it would '
            'return every firm''s counts to any caller';
    END IF;

    SELECT count(*) INTO n FROM pg_class c
    JOIN pg_namespace ns ON ns.oid = c.relnamespace
    WHERE ns.nspname = 'public' AND c.relkind = 'r';
    RAISE NOTICE 'init.sql OK: % tables, RLS forced, auditor_app/auditor_sys created', n;
END
$selfcheck$;