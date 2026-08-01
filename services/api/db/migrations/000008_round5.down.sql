DROP VIEW IF EXISTS book_automation_rate;
DROP TABLE IF EXISTS entity_tags;
DROP TABLE IF EXISTS tags;
DROP TABLE IF EXISTS bank_holidays;
DROP TABLE IF EXISTS config_change_log;
ALTER TABLE reconciliation_groups DROP CONSTRAINT IF EXISTS reconciliation_groups_status_check;
ALTER TABLE reconciliation_groups ADD CONSTRAINT reconciliation_groups_status_check
    CHECK (status IN ('auto_linked','needs_review','confirmed','rejected'));
ALTER TABLE extracted_entities DROP COLUMN IF EXISTS status;
ALTER TABLE extracted_entities DROP COLUMN IF EXISTS corrects_entity_id;
ALTER TABLE extracted_entities DROP COLUMN IF EXISTS manually_created_by;
ALTER TABLE extracted_entities DROP COLUMN IF EXISTS created_by;
