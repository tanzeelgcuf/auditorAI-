-- AI Auditor v1 — Database Initialization (DDL from docs 05-10)
-- This runs once on first container startup

-- ===== EXTENSIONS =====
CREATE EXTENSION IF NOT EXISTS "uuid-ossp";
CREATE EXTENSION IF NOT EXISTS "pgvector";

-- ===== TENANCY =====
CREATE TABLE firms (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name TEXT NOT NULL,
    stripe_customer_id TEXT,
    logo_storage_key TEXT,
    brand_primary_color TEXT DEFAULT '#0F172A',
    report_footer_text TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

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
    totp_secret TEXT,
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
    extracted_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ===== CROSS-LINKING & REVIEW =====
CREATE TABLE reconciliation_groups (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    client_book_id UUID NOT NULL REFERENCES client_books(id),
    link_confidence NUMERIC(4,3) NOT NULL,
    status TEXT NOT NULL DEFAULT 'auto_linked' CHECK (status IN ('auto_linked','needs_review','confirmed','rejected')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

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
    revoked_at TIMESTAMPTZ
);

CREATE TABLE webhook_subscriptions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    firm_id UUID NOT NULL REFERENCES firms(id),
    target_url TEXT NOT NULL,
    event_types TEXT[] NOT NULL,
    signing_secret TEXT NOT NULL,
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
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ===== COA TEMPLATES =====
CREATE TABLE coa_templates (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    template_name TEXT NOT NULL,
    industry TEXT,
    accounts JSONB NOT NULL
);

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