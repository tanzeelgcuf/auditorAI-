-- Doc 11 (Round 5): human override capability + matching precision.
-- Manual entity creation, group supersession, config audit trail,
-- business-day matching (holidays), custom tags.

-- 1. Manual entity creation (§1)
ALTER TABLE extracted_entities ADD COLUMN created_by TEXT NOT NULL DEFAULT 'system'
    CHECK (created_by IN ('system', 'manual'));
ALTER TABLE extracted_entities ADD COLUMN manually_created_by UUID REFERENCES users(id);
ALTER TABLE extracted_entities ADD COLUMN corrects_entity_id UUID REFERENCES extracted_entities(id);
ALTER TABLE extracted_entities ADD COLUMN status TEXT NOT NULL DEFAULT 'active'
    CHECK (status IN ('active', 'superseded_by_manual'));

-- 2. Reconciliation group split/merge (§2) — superseded groups preserve history.
ALTER TABLE reconciliation_groups DROP CONSTRAINT IF EXISTS reconciliation_groups_status_check;
ALTER TABLE reconciliation_groups ADD CONSTRAINT reconciliation_groups_status_check
    CHECK (status IN ('auto_linked','needs_review','confirmed','rejected','superseded'));

-- 3. Configuration change audit trail (§3)
CREATE TABLE config_change_log (
    id BIGSERIAL PRIMARY KEY,
    client_book_id UUID NOT NULL REFERENCES client_books(id),
    changed_by UUID NOT NULL REFERENCES users(id),
    field_name TEXT NOT NULL,
    old_value TEXT,
    new_value TEXT,
    changed_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 4. Business-day-aware matching (§4)
CREATE TABLE bank_holidays (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    client_book_id UUID,
    holiday_date DATE NOT NULL,
    description TEXT
);

-- 6. Custom tags (§6)
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

-- 5. Automation-rate view (§5)
CREATE VIEW book_automation_rate AS
SELECT
    client_book_id,
    COUNT(*) FILTER (WHERE status = 'auto_linked') AS auto_linked_count,
    COUNT(*) FILTER (WHERE status = 'needs_review') AS needs_review_count,
    COUNT(*) FILTER (WHERE status = 'confirmed') AS confirmed_count,
    COUNT(*) AS total_count
FROM reconciliation_groups
GROUP BY client_book_id;
