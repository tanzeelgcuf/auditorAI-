package documents

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/middleware"
	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/pipeline"
)

const maxUploadSize = 25 * 1024 * 1024 // 25MB per doc 06 §5

var allowedDocTypes = map[string]string{
	".pdf":  "invoice", // default; refined at extraction by content
	".png":  "invoice",
	".jpg":  "invoice",
	".jpeg": "invoice",
	".csv":  "gl_export",
	".xlsx": "gl_export",
	".ofx":  "bank_statement",
	".qfx":  "bank_statement",
}

type Service struct {
	db        *pgxpool.Pool
	pipeline  *pipeline.EventClient
}

func NewService() *Service { return &Service{} }

func (s *Service) SetDB(db *pgxpool.Pool)          { s.db = db }
func (s *Service) SetPipeline(p *pipeline.EventClient) { s.pipeline = p }

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeProblem(w http.ResponseWriter, status int, typ, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"type": typ, "title": http.StatusText(status), "status": status, "detail": detail,
	})
}

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

// HandleUpload accepts a document, stores it, and enqueues ingestion.
// Supports PDF/PNG/JPG (OCR path) and CSV/XLSX/OFX/QFX (structured path).
func (s *Service) HandleUpload(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("bookId")
	if bookID == "" || !contains(middleware.GetAssignedBooks(r.Context()), bookID) {
		writeProblem(w, http.StatusNotFound, "https://ai-auditor.dev/errors/not-found", "book not found")
		return
	}
	userID := middleware.GetUserID(r.Context())

	// Enforce upload size before parsing multipart
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)

	file, header, err := r.FormFile("file")
	if err != nil {
		if errors.Is(err, http.ErrMissingFile) {
			writeProblem(w, http.StatusBadRequest, "https://ai-auditor.dev/errors/bad-request", "file field required")
			return
		}
		writeProblem(w, http.StatusRequestEntityTooLarge, "https://ai-auditor.dev/errors/too-large", "file exceeds 25MB")
		return
	}
	defer file.Close()

	ext := strings.ToLower(path.Ext(header.Filename))
	docType, ok := allowedDocTypes[ext]
	if !ok {
		writeProblem(w, http.StatusUnsupportedMediaType, "https://ai-auditor.dev/errors/unsupported", "supported: pdf, png, jpg, csv, xlsx, ofx, qfx")
		return
	}

	data, err := io.ReadAll(file)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "read failed")
		return
	}

	// Content hash for duplicate detection (doc 07 §3)
	hash := sha256.Sum256(data)
	contentHash := hex.EncodeToString(hash[:])

	c := middleware.GetConn(r.Context())
	if c == nil {
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "no db conn")
		return
	}

	// Duplicate check within the same book -> 409 (doc 07 §3)
	var existingID string
	err = c.QueryRow(r.Context(),
		`SELECT id::text FROM source_documents
		 WHERE client_book_id = $1 AND content_hash = $2 AND deleted_at IS NULL`,
		bookID, contentHash).Scan(&existingID)
	if err == nil {
		writeProblem(w, http.StatusConflict, "https://ai-auditor.dev/errors/duplicate",
			"a document with identical content already exists in this book")
		return
	} else if !errors.Is(err, pgx.ErrNoRows) {
		slog.Error("duplicate check failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "query failed")
		return
	}

	storageKey := fmt.Sprintf("documents/%s/%s-%s", bookID, uuid.NewString(), header.Filename)
	// In production: upload data to S3/MinIO here (aws-sdk-go). For now, persist metadata.

	var docID string
	err = c.QueryRow(r.Context(),
		`INSERT INTO source_documents
			(client_book_id, filename, doc_type, storage_key, content_hash, uploaded_by, ocr_status)
		 VALUES ($1, $2, $3, $4, $5, $6, 'pending')
		 RETURNING id::text`,
		bookID, header.Filename, docType, storageKey, contentHash, userID).Scan(&docID)
	if err != nil {
		slog.Error("failed to insert document", "error", err)
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "insert failed")
		return
	}

	// Publish ingestion event so services/ingestion processes this doc
	if s.pipeline != nil {
		payload, _ := json.Marshal(map[string]string{
			"document_id": docID, "storage_key": storageKey, "doc_type": docType,
			"client_book_id": bookID,
		})
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		_, pubErr := s.pipeline.Publish(ctx, "document.uploaded", payload)
		if pubErr != nil {
			slog.Error("failed to publish upload event", "error", pubErr)
		}
	}

	slog.Info("document uploaded", "doc_id", docID, "book_id", bookID)
	body, _ := middleware.EncodeJSON(map[string]interface{}{
		"id": docID, "client_book_id": bookID, "filename": header.Filename,
		"doc_type": docType, "ocr_status": "pending",
	})
	// Store idempotent response (non-fatal on failure — retry would reprocess)
	middleware.StoreIdempotentResponse(r.Context(), s.db, userID,
		r.Header.Get("Idempotency-Key"), http.StatusCreated, body)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	w.Write(body)
}

func (s *Service) HandleList(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("bookId")
	if bookID == "" || !contains(middleware.GetAssignedBooks(r.Context()), bookID) {
		writeProblem(w, http.StatusNotFound, "https://ai-auditor.dev/errors/not-found", "book not found")
		return
	}

	c := middleware.GetConn(r.Context())
	if c == nil {
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "no db conn")
		return
	}

	rows, err := c.Query(r.Context(),
		`SELECT id::text, client_book_id::text, filename, doc_type, storage_key, content_hash,
			COALESCE(page_count, 0), uploaded_by::text, ocr_status, uploaded_at
		 FROM source_documents
		 WHERE client_book_id = $1 AND deleted_at IS NULL
		 ORDER BY uploaded_at DESC LIMIT 100`, bookID)
	if err != nil {
		slog.Error("failed to list documents", "error", err)
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "query failed")
		return
	}
	defer rows.Close()

	type doc struct {
		ID           string    `json:"id"`
		ClientBookID string    `json:"client_book_id"`
		Filename     string    `json:"filename"`
		DocType      string    `json:"doc_type"`
		StorageKey   string    `json:"storage_key"`
		ContentHash  string    `json:"content_hash"`
		PageCount    int       `json:"page_count"`
		UploadedBy   string    `json:"uploaded_by"`
		OcrStatus    string    `json:"ocr_status"`
		UploadedAt   time.Time `json:"uploaded_at"`
	}
	var out []doc
	for rows.Next() {
		var d doc
		if err := rows.Scan(&d.ID, &d.ClientBookID, &d.Filename, &d.DocType, &d.StorageKey,
			&d.ContentHash, &d.PageCount, &d.UploadedBy, &d.OcrStatus, &d.UploadedAt); err != nil {
			continue
		}
		out = append(out, d)
	}
	if out == nil {
		out = []doc{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"items": out, "next_cursor": nil})
}

func (s *Service) HandleGet(w http.ResponseWriter, r *http.Request) {
	docID := r.PathValue("docId")
	if docID == "" {
		writeProblem(w, http.StatusBadRequest, "https://ai-auditor.dev/errors/bad-request", "docId required")
		return
	}

	c := middleware.GetConn(r.Context())
	if c == nil {
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "no db conn")
		return
	}

	var id, bookID, filename, docType, status string
	err := c.QueryRow(r.Context(),
		`SELECT id::text, client_book_id::text, filename, doc_type, ocr_status
		 FROM source_documents WHERE id = $1 AND deleted_at IS NULL`, docID).
		Scan(&id, &bookID, &filename, &docType, &status)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeProblem(w, http.StatusNotFound, "https://ai-auditor.dev/errors/not-found", "document not found")
			return
		}
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "query failed")
		return
	}
	// RLS on source_documents already scopes to assigned books; if the caller
	// isn't assigned, the row never appears -> 404 (no existence leak).

	middleware.RecordAccess(r.Context(), s.db, middleware.GetUserID(r.Context()), bookID, "view_document", docID)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"id": id, "client_book_id": bookID, "filename": filename,
		"doc_type": docType, "ocr_status": status,
	})
}

func (s *Service) HandlePresignedView(w http.ResponseWriter, r *http.Request) {
	docID := r.PathValue("docId")
	if docID == "" {
		writeProblem(w, http.StatusBadRequest, "https://ai-auditor.dev/errors/bad-request", "docId required")
		return
	}

	c := middleware.GetConn(r.Context())
	if c == nil {
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "no db conn")
		return
	}

	var storageKey, clientBookID string
	err := c.QueryRow(r.Context(),
		`SELECT storage_key, client_book_id::text FROM source_documents WHERE id = $1 AND deleted_at IS NULL`, docID).
		Scan(&storageKey, &clientBookID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeProblem(w, http.StatusNotFound, "https://ai-auditor.dev/errors/not-found", "document not found")
			return
		}
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "query failed")
		return
	}

	// In production: generate a presigned S3/MinIO URL for the storage key.
	// For now, return the key so the web app can resolve it.
	middleware.RecordAccess(r.Context(), s.db, middleware.GetUserID(r.Context()), clientBookID, "view_document", docID)
	writeJSON(w, http.StatusOK, map[string]interface{}{"storage_key": storageKey})
}
