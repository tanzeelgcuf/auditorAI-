// The sync worker: pulls the provider's data and turns it into pipeline
// entities. The connector is another ingestion SOURCE: the entities land in
// extracted_entities (the same tables, the same shapes), so linking,
// verification, and reporting are unchanged. Idempotency: a re-synced entity
// carries the same external_ref (the provider's own id) and a UNIQUE partial
// index makes the re-insert a no-op — the first pull wins, so a re-sync
// cannot duplicate legs and cannot change a group's totals.
//
// The sync publishes ONLY link.requested, deliberately: the provider's data
// is already structured and typed, so the LLM extraction pass would be
// redundant — unlike the file-upload path, where the LLM refines types. The
// book-wide link pass is deterministic and runs on the pending set.
package connectors

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/middleware"
)

// entityRecord is one provider record mapped into the pipeline's entity
// shape — the same fields extracted_entities holds.
type entityRecord struct {
	ExternalRef     string
	EntityType      string
	AmountCents     int64
	Currency        string
	TransactionDate string // YYYY-MM-DD or ""
	Counterparty    string
	Description     string
	AccountCode     string
	DebitOrCredit   string
}

// decimalToCents converts a provider's decimal amount string to integer
// cents WITHOUT float, MIRRORING the Rust parser's normalize_decimal
// (structured.rs:152-236) — the parity is the point: an amount the Rust
// rejects, this must reject too, or a candidate Rust would reconcile is
// silently parsed differently here. The rules it mirrors: both separator
// types present → the RIGHTMOST is the decimal ("1.234,56" / "1,234.56"); one
// type repeated with no decimal evidence → reject (even "1,234,567", whose
// grouping is valid — the convention is not decidable there); one separator →
// the tail's length decides, 3+ trailing digits reject as indistinguishable;
// the integer part must be valid thousands grouping against the OTHER
// separator; >2 decimal places reject (choosing a rounding is a financial
// calculation this service must not make); an empty integer part with a
// decimal is legitimate (".99" = 99 cents).
//
// This is a unit conversion in Go, which the architecture normally routes to
// Rust — it lives here because the connector's data is already structured (no
// file to parse) and the operation is a string parse of a decimal literal,
// the same one ingestion's parse_amount performs. A THIRD money-parse
// implementation is the parity-guard class: check_amount_parity.py covers
// Rust↔Python; this Go one is its sibling and should join a parity check when
// it earns one.
func decimalToCents(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty amount")
	}
	neg := false
	if strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	} else if strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")") {
		neg = true
		s = s[1 : len(s)-1]
	}

	dots := strings.Count(s, ".")
	commas := strings.Count(s, ",")
	if dots > 0 && commas > 0 {
		// Both present: the RIGHTMOST is the decimal point.
		if strings.LastIndex(s, ".") > strings.LastIndex(s, ",") {
			return decToCents(s, '.', neg)
		}
		return decToCents(s, ',', neg)
	}
	if dots+commas == 0 {
		// bare integer dollars
		w, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return 0, errors.New("malformed amount: " + s)
		}
		cents := w * 100
		if neg {
			cents = -cents
		}
		return cents, nil
	}
	sep, n := byte('.'), dots
	if dots == 0 {
		sep, n = byte(','), commas
	}
	if n > 1 {
		// Repeated single separator with no decimal evidence: reject — the
		// convention is not decidable there, and the Rust parser rejects.
		return 0, errors.New("amount has a repeated separator and no decimal point: " + s)
	}
	// One separator: the tail's length decides. 1-2 trailing digits: a decimal
	// point. 3+: indistinguishable from a thousands separator — reject.
	tail := s[strings.LastIndexByte(s, sep)+1:]
	if len(tail) < 1 || len(tail) > 2 {
		return 0, errors.New("amount's separator is ambiguous: " + s)
	}
	return decToCents(s, sep, neg)
}

func decToCents(s string, dec byte, neg bool) (int64, error) {
	idx := strings.LastIndexByte(s, dec)
	intRaw := s[:idx]
	frac := s[idx+1:]
	// Whatever is not the decimal separator must be valid thousands grouping,
	// checked against the OTHER separator (the Rust parser's rule).
	thou := byte(',')
	if dec == ',' {
		thou = byte('.')
	}
	if intRaw != "" && !validGrouping(intRaw, thou) {
		return 0, errors.New("amount's grouping is invalid: " + s)
	}
	// More than 2 decimal places cannot be represented in cents, and choosing
	// a rounding for the operator is a financial calculation this service must
	// not make.
	if len(frac) > 2 || !allDigits(frac) {
		return 0, errors.New("malformed amount: " + s)
	}
	if len(frac) == 1 {
		// The Rust parser pads "5" -> "50" ({:0<2}, its own comment): a
		// 1-digit fraction means tenths, so 1500.5 is 150050 cents, not
		// 150005. Caught by the port parity check, not by reasoning.
		frac += "0"
	}
	intPart := digitsOnly(intRaw)
	if intPart == "" {
		intPart = "0"
	}
	w, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return 0, errors.New("malformed amount: " + s)
	}
	f, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0, errors.New("malformed amount: " + s)
	}
	cents := w*100 + f
	if neg {
		cents = -cents
	}
	return cents, nil
}

func validGrouping(s string, sep byte) bool {
	groups := strings.Split(s, string(sep))
	for i, g := range groups {
		if i > 0 && len(g) != 3 {
			return false
		}
		for _, c := range g {
			if c < '0' || c > '9' {
				return false
			}
		}
	}
	return true
}

func digitsOnly(s string) string {
	var b strings.Builder
	for _, c := range s {
		if c >= '0' && c <= '9' {
			b.WriteRune(c)
		}
	}
	return b.String()
}

func allDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// HandleSync pulls the provider's data for this book and writes it into the
// pipeline. Behind the authenticator; the reads and writes run on the
// request's RLS-primed connection (rule 14) — everything is book-scoped.
// Each entity type is written in its own transaction (the synthetic source
// doc + its entities), so a failure leaves no half-written type.
func (s *Service) HandleSync(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("bookId")
	provider := Provider(r.PathValue("provider"))
	userID := middleware.GetUserID(r.Context())

	assigned := middleware.GetAssignedBooks(r.Context())
	found := false
	for _, b := range assigned {
		if b == bookID {
			found = true
			break
		}
	}
	if bookID == "" || !found {
		writeProblem(w, http.StatusNotFound, "https://ai-auditor.dev/errors/not-found", "book not found")
		return
	}

	cfg, err := configFor(provider)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "https://ai-auditor.dev/errors/bad-request", "unknown provider")
		return
	}
	clientID, clientSecret, err := credentialsFor(provider)
	if err != nil || clientID == "" {
		writeProblem(w, http.StatusServiceUnavailable, "https://ai-auditor.dev/errors/not-configured",
			"connector not configured: OAuth client credentials missing")
		return
	}

	c := middleware.GetConn(r.Context())
	if c == nil {
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "no db conn")
		return
	}

	var connID, providerAccount, encAccess, encRefresh string
	var tokenExpires *time.Time
	err = c.QueryRow(r.Context(),
		`SELECT id::text, provider_account_id, encrypted_access_token, encrypted_refresh_token, token_expires_at
		 FROM connector_connections WHERE client_book_id = $1 AND provider = $2`,
		bookID, string(provider)).Scan(&connID, &providerAccount, &encAccess, &encRefresh, &tokenExpires)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeProblem(w, http.StatusNotFound, "https://ai-auditor.dev/errors/not-found",
				"no connection for this provider — authorize first")
			return
		}
		slog.Error("sync: connection read failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "query failed")
		return
	}

	accessToken, err := decryptToken(encAccess)
	if err != nil {
		slog.Error("sync: access token decrypt failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "token decrypt failed")
		return
	}
	refreshToken, err := decryptToken(encRefresh)
	if err != nil {
		slog.Error("sync: refresh token decrypt failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "token decrypt failed")
		return
	}

	// Refresh first if expired: QBO expires refresh tokens after 100 days of
	// inactivity, so every sync refreshes — the activity keeps the token
	// alive. The refreshed tokens are re-encrypted and stored on the primed
	// connection (the row is this book's; the policy applies).
	if tokenExpires == nil || time.Now().After(*tokenExpires) {
		tok, rerr := s.refreshAccessToken(r.Context(), cfg, clientID, clientSecret, refreshToken)
		if rerr != nil {
			slog.Error("sync: token refresh failed", "provider", string(provider), "error", rerr)
			writeProblem(w, http.StatusBadGateway, "https://ai-auditor.dev/errors/upstream", "token refresh failed")
			return
		}
		encA, eerr := encryptToken(tok.AccessToken)
		if eerr != nil {
			writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "token encryption failed")
			return
		}
		encR, eerr := encryptToken(tok.RefreshToken)
		if eerr != nil {
			writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "token encryption failed")
			return
		}
		var newExp *time.Time
		if tok.ExpiresIn > 0 {
			t := time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
			newExp = &t
		}
		if _, err := c.Exec(r.Context(),
			`UPDATE connector_connections
			    SET encrypted_access_token = $1, encrypted_refresh_token = $2, token_expires_at = $3
			  WHERE client_book_id = $4 AND provider = $5`,
			encA, encR, newExp, bookID, string(provider)); err != nil {
			slog.Error("sync: refreshed token persist failed", "error", err)
			writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "connection update failed")
			return
		}
		accessToken = tok.AccessToken
	}

	var records []entityRecord
	switch provider {
	case ProviderQuickBooks:
		records, err = s.pullQuickBooks(r.Context(), cfg, providerAccount, accessToken)
	case ProviderXero:
		records, err = s.pullXero(r.Context(), cfg, accessToken)
	}
	if err != nil {
		slog.Error("sync: provider pull failed", "provider", string(provider), "error", err)
		writeProblem(w, http.StatusBadGateway, "https://ai-auditor.dev/errors/upstream", "provider pull failed")
		return
	}

	// Group by entity type; each type is written in its own transaction.
	byType := map[string][]entityRecord{}
	order := []string{}
	for _, rec := range records {
		if _, seen := byType[rec.EntityType]; !seen {
			order = append(order, rec.EntityType)
		}
		byType[rec.EntityType] = append(byType[rec.EntityType], rec)
	}

	written := 0
	skipped := 0
	now := time.Now().UTC()
	for _, entityType := range order {
		n, sk, werr := s.writeEntities(r.Context(), c, bookID, provider, userID, entityType, byType[entityType], now)
		if werr != nil {
			slog.Error("sync: entity write failed", "entity_type", entityType, "error", werr)
			writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "entity write failed")
			return
		}
		written += n
		skipped += sk
	}

	if _, err := c.Exec(r.Context(),
		`UPDATE connector_connections SET last_synced_at = now() WHERE client_book_id = $1 AND provider = $2`,
		bookID, string(provider)); err != nil {
		slog.Warn("sync: last_synced_at update failed", "error", err)
	}

	// The book-wide link pass: the same event the coordinator publishes after
	// entity persistence. Deterministic (no LLM) and runs on the pending set,
	// so re-running it is cheap.
	if s.pipeline != nil {
		if _, err := s.pipeline.Publish(r.Context(), "link.requested",
			[]byte(`{"client_book_id":"`+bookID+`"}`)); err != nil {
			slog.Warn("sync: link event publish failed", "error", err)
		}
	}

	slog.Info("connector sync complete", "provider", string(provider), "book", bookID,
		"written", written, "skipped", skipped)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"client_book_id": bookID, "provider": string(provider),
		"entities_written": written, "entities_skipped": skipped,
	})
}

// writeEntities writes one entity type's records inside one transaction: the
// synthetic source document (found-or-created by its filename, so a same-day
// re-sync reuses it) plus the entities, deduped by external_ref via ON
// CONFLICT DO NOTHING.
func (s *Service) writeEntities(ctx context.Context, c *pgxpool.Conn, bookID string, provider Provider, userID, entityType string, records []entityRecord, now time.Time) (int, int, error) {
	tx, err := c.Begin(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	docID, err := ensureSyncDoc(ctx, tx, bookID, provider, userID, entityType, now)
	if err != nil {
		return 0, 0, err
	}

	written, skipped := 0, 0
	for _, rec := range records {
		if rec.ExternalRef == "" {
			skipped++ // no provider id: the dedupe would be meaningless
			continue
		}
		tag, err := tx.Exec(ctx,
			`INSERT INTO extracted_entities
				(client_book_id, source_document_id, entity_type, amount_cents, currency,
				 transaction_date, counterparty, description, gl_account_code,
				 external_ref, page_number, bbox, extraction_confidence, source_format)
			 VALUES ($1, $2, $3, $4, $5, NULLIF($6,'')::date, NULLIF($7,''), NULLIF($8,''), NULLIF($9,''),
				$10, 1, $11, 1.0, 'structured')
			 ON CONFLICT (client_book_id, external_ref) DO NOTHING`,
			bookID, docID, rec.EntityType, rec.AmountCents, rec.Currency,
			rec.TransactionDate, rec.Counterparty, rec.Description, rec.AccountCode,
			rec.ExternalRef, `{"x":0,"y":0,"width":0,"height":0}`)
		if err != nil {
			return 0, 0, err
		}
		if tag.RowsAffected() == 1 {
			written++
		} else {
			skipped++ // already synced: the first pull wins
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, 0, err
	}
	return written, skipped, nil
}

// ensureSyncDoc finds or creates the sync's synthetic source document. One
// per sync per entity type: the entities' citation points at a real row
// (rule 3), the doc_type matches the entity type so the schema's CHECK is
// satisfied, and the storage_key is a synthetic pointer to the provider's
// data — not local storage, so the storage-key-orphan guard's allowlist
// carries this reason.
func ensureSyncDoc(ctx context.Context, tx pgx.Tx, bookID string, provider Provider, userID, entityType string, now time.Time) (string, error) {
	docType := entityTypeToDocType(entityType)
	filename := "connector-" + string(provider) + "-" + now.Format("2006-01-02") + "-" + entityType
	var docID string
	err := tx.QueryRow(ctx,
		`SELECT id::text FROM source_documents
		 WHERE client_book_id = $1 AND filename = $2 AND deleted_at IS NULL`,
		bookID, filename).Scan(&docID)
	if err == nil {
		return docID, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	storageKey := "connector://" + string(provider) + "/" + bookID + "/" + now.Format("2006-01-02") + "/" + entityType
	sum := sha256Hex(string(provider) + "|" + now.Format("2006-01-02") + "|" + entityType)
	err = tx.QueryRow(ctx,
		`INSERT INTO source_documents
			(client_book_id, filename, doc_type, storage_key, content_hash, uploaded_by, ocr_status)
		 VALUES ($1, $2, $3, $4, $5, $6, 'done')
		 RETURNING id::text`,
		bookID, filename, docType, storageKey, sum, userID).Scan(&docID)
	if err != nil {
		return "", err
	}
	return docID, nil
}

// entityTypeToDocType maps a DB entity type to the synthetic source
// document's doc_type — the schema's CHECK constraint allows exactly these
// three, and the doc_type on a connector doc is bookkeeping for traceability,
// not ingestion routing (nothing re-parses this file).
func entityTypeToDocType(entityType string) string {
	switch entityType {
	case "invoice_line_item":
		return "invoice"
	case "bank_transaction":
		return "bank_statement"
	default:
		return "gl_export"
	}
}
