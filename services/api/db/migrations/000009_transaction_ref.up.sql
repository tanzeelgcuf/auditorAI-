-- Doc 08: source transaction reference for structured entities (GL "Num", OFX "FITID").
ALTER TABLE extracted_entities ADD COLUMN transaction_ref TEXT;
CREATE INDEX idx_extracted_entities_ref ON extracted_entities (client_book_id, transaction_ref);
