-- Doc 12 §2 / Round 7: reconcile AP and AR activity separately. Groups get a
-- scope derived from their GL leg's chart-of-accounts account type; nothing is
-- excluded, just categorized (the row-3 deposit is a real bank↔GL match, not a
-- false positive — it belongs in an 'ar' section, not dropped).
ALTER TABLE reconciliation_groups ADD COLUMN group_scope TEXT NOT NULL DEFAULT 'ap'
    CHECK (group_scope IN ('ap', 'ar', 'other'));
CREATE INDEX idx_reconciliation_groups_scope ON reconciliation_groups (client_book_id, group_scope);
