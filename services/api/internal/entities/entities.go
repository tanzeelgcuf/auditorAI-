package entities

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/middleware"
)

type Service struct {
	db *pgxpool.Pool
}

func NewService() *Service { return &Service{} }

func (s *Service) SetDB(db *pgxpool.Pool) { s.db = db }

func writeProblem(w http.ResponseWriter, status int, typ, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"type": typ, "title": http.StatusText(status), "status": status, "detail": detail,
	})
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

// HandleList returns extracted entities for a book, filterable by type/date/source_format.
func (s *Service) HandleList(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("bookId")
	if bookID == "" || !contains(middleware.GetAssignedBooks(r.Context()), bookID) {
		writeProblem(w, http.StatusNotFound, "https://ai-auditor.dev/errors/not-found", "book not found")
		return
	}

	entityType := r.URL.Query().Get("entity_type")
	fromDate := r.URL.Query().Get("from")
	toDate := r.URL.Query().Get("to")
	sourceFormat := r.URL.Query().Get("source_format")
	limit := 100

	query := `SELECT id::text, client_book_id::text, source_document_id::text, entity_type,
		entity_subtype, amount_cents, currency, COALESCE(transaction_date, 'epoch'),
		COALESCE(counterparty, ''), COALESCE(description, ''), COALESCE(gl_account_code, ''),
		page_number, bbox, extraction_confidence, source_format, extracted_at
		FROM extracted_entities WHERE client_book_id = $1`
	args := []interface{}{bookID}
	if entityType != "" {
		args = append(args, entityType)
		query += " AND entity_type = $" + itoa(len(args))
	}
	if fromDate != "" {
		args = append(args, fromDate)
		query += " AND transaction_date >= $" + itoa(len(args))
	}
	if toDate != "" {
		args = append(args, toDate)
		query += " AND transaction_date <= $" + itoa(len(args))
	}
	if sourceFormat != "" {
		args = append(args, sourceFormat)
		query += " AND source_format = $" + itoa(len(args))
	}
	query += " ORDER BY transaction_date DESC NULLS LAST LIMIT " + itoa(limit)

	c := middleware.GetConn(r.Context())
	if c == nil {
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "no db conn")
		return
	}

	rows, err := c.Query(r.Context(), query, args...)
	if err != nil {
		slog.Error("failed to list entities", "error", err)
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "query failed")
		return
	}
	defer rows.Close()

	type entity struct {
		ID                  string    `json:"id"`
		ClientBookID        string    `json:"client_book_id"`
		SourceDocumentID    string    `json:"source_document_id"`
		EntityType          string    `json:"entity_type"`
		EntitySubtype       string    `json:"entity_subtype"`
		AmountCents         int64     `json:"amount_cents"`
		Currency            string    `json:"currency"`
		TransactionDate     string    `json:"transaction_date"`
		Counterparty        string    `json:"counterparty"`
		Description         string    `json:"description"`
		GLAccountCode       string    `json:"gl_account_code"`
		PageNumber          int       `json:"page_number"`
		BBox                map[string]float64 `json:"bbox"`
		ExtractionConfidence float64  `json:"extraction_confidence"`
		SourceFormat        string    `json:"source_format"`
		ExtractedAt         time.Time `json:"extracted_at"`
	}

	var out []entity
	for rows.Next() {
		var e entity
		var txnDate time.Time
		var bboxJSON []byte
		if err := rows.Scan(&e.ID, &e.ClientBookID, &e.SourceDocumentID, &e.EntityType,
			&e.EntitySubtype, &e.AmountCents, &e.Currency, &txnDate, &e.Counterparty,
			&e.Description, &e.GLAccountCode, &e.PageNumber, &bboxJSON,
			&e.ExtractionConfidence, &e.SourceFormat, &e.ExtractedAt); err != nil {
			slog.Warn("failed to scan entity row", "error", err)
			continue
		}
		if !txnDate.IsZero() {
			e.TransactionDate = txnDate.Format("2006-01-02")
		}
		e.BBox = map[string]float64{}
		if len(bboxJSON) > 0 {
			_ = json.Unmarshal(bboxJSON, &e.BBox)
		}
		out = append(out, e)
	}
	if out == nil {
		out = []entity{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"items": out, "next_cursor": nil})
}

func itoa(n int) string {
	return strconv.Itoa(n)
}
