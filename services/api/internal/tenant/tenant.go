package tenant

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/middleware"
)

type Service struct {
	db *pgxpool.Pool
}

func NewService() *Service {
	return &Service{}
}

func (s *Service) SetDB(db *pgxpool.Pool) {
	s.db = db
}

func (s *Service) HandleCreateBook(w http.ResponseWriter, r *http.Request) {
	userID := middleware.GetUserID(r.Context())
	firmID := middleware.GetFirmID(r.Context())
	role := middleware.GetRole(r.Context())
	if userID == "" || firmID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	var req struct {
		ClientName string `json:"client_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if req.ClientName == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "client_name is required"})
		return
	}

	conn := middleware.GetConn(r.Context())
	var driver *pgxpool.Conn
	if conn != nil {
		driver = conn
	} else {
		c, err := s.db.Acquire(r.Context())
		if err != nil {
			slog.Error("failed to acquire connection", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		defer c.Release()
		driver = c
	}

	tx, err := driver.Begin(r.Context())
	if err != nil {
		slog.Error("failed to begin transaction", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	defer tx.Rollback(r.Context())

	var bookID string
	err = tx.QueryRow(r.Context(),
		`INSERT INTO client_books (firm_id, client_name) VALUES ($1, $2) RETURNING id`,
		firmID, req.ClientName).Scan(&bookID)
	if err != nil {
		slog.Error("failed to create book", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create book"})
		return
	}

	// Assign firm_admin as owner
	_, err = tx.Exec(r.Context(),
		`INSERT INTO user_book_assignments (user_id, client_book_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
		userID, bookID)
	if err != nil {
		slog.Error("failed to assign admin to book", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to assign book owner"})
		return
	}

	if err := tx.Commit(r.Context()); err != nil {
		slog.Error("failed to commit book creation", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	// TODO: assign all firm_admins to this book automatically
	// ponytail: only the creating admin is assigned now; add when multi-admin firm setup is built

	slog.Info("book created", "book_id", bookID, "firm_id", firmID, "user_id", userID)
	if role == "firm_admin" {
		writeJSON(w, http.StatusCreated, map[string]string{
			"id": bookID, "client_name": req.ClientName,
		})
	} else {
		writeJSON(w, http.StatusCreated, map[string]string{
			"id": bookID, "client_name": req.ClientName,
		})
	}
}

func (s *Service) HandleListBooks(w http.ResponseWriter, r *http.Request) {
	userID := middleware.GetUserID(r.Context())
	firmID := middleware.GetFirmID(r.Context())
	if userID == "" || firmID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	assignedBooks := middleware.GetAssignedBooks(r.Context())
	if len(assignedBooks) == 0 {
		writeJSON(w, http.StatusOK, []interface{}{})
		return
	}

	// Use the RLS connection if available so app.current_firm filters apply
	conn := middleware.GetConn(r.Context())
	if conn != nil {
		rows, err := conn.Query(r.Context(),
			"SELECT id::text, client_name FROM client_books WHERE id = ANY($1)", assignedBooks)
		if err != nil {
			slog.Error("failed to list books", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		defer rows.Close()
		var books []map[string]string
		for rows.Next() {
			var id, name string
			if err := rows.Scan(&id, &name); err != nil {
				slog.Error("failed to scan book row", "error", err)
				continue
			}
			books = append(books, map[string]string{"id": id, "client_name": name})
		}
		if books == nil {
			books = []map[string]string{}
		}
		writeJSON(w, http.StatusOK, books)
		return
	}

	// Fallback: direct pool query via firm_id
	rows, err := s.db.Query(r.Context(),
		"SELECT id::text, client_name FROM client_books WHERE firm_id = $1 AND id = ANY($2)",
		firmID, assignedBooks)
	if err != nil {
		slog.Error("failed to list books", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	defer rows.Close()
	var books []map[string]string
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			slog.Error("failed to scan book row", "error", err)
			continue
		}
		books = append(books, map[string]string{"id": id, "client_name": name})
	}
	if books == nil {
		books = []map[string]string{}
	}
	writeJSON(w, http.StatusOK, books)
}

func (s *Service) HandleGetBook(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("bookId")
	if bookID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bookId is required"})
		return
	}

	assignedBooks := middleware.GetAssignedBooks(r.Context())
	if !contains(assignedBooks, bookID) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "book not found"})
		return
	}

	conn := middleware.GetConn(r.Context())
	var clientName string
	var err error

	if conn != nil {
		err = conn.QueryRow(r.Context(),
			"SELECT client_name FROM client_books WHERE id = $1", bookID).Scan(&clientName)
	} else {
		err = s.db.QueryRow(r.Context(),
			"SELECT client_name FROM client_books WHERE id = $1", bookID).Scan(&clientName)
	}

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "book not found"})
			return
		}
		slog.Error("failed to get book", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"id": bookID, "client_name": clientName})
}

func (s *Service) HandleUpdateBookSettings(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("bookId")
	if bookID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bookId is required"})
		return
	}

	assignedBooks := middleware.GetAssignedBooks(r.Context())
	if !contains(assignedBooks, bookID) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "book not found"})
		return
	}

	var settings struct {
		ClientName                   *string  `json:"client_name"`
		BaseCurrency                 *string  `json:"base_currency"`
		FiscalYearStartMonth         *int     `json:"fiscal_year_start_month"`
		AutoLinkConfidenceThreshold  *float64 `json:"auto_link_confidence_threshold"`
		ReviewConfidenceFloor        *float64 `json:"review_confidence_floor"`
	}
	if err := json.NewDecoder(r.Body).Decode(&settings); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	conn := middleware.GetConn(r.Context())
	db := conn
	if db == nil {
		c, err := s.db.Acquire(r.Context())
		if err != nil {
			slog.Error("failed to acquire connection", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		defer c.Release()
		db = c
	}

	// Build dynamic update — for simplicity update all settable fields if provided
	// ponytail: use a proper column list builder when settings grow beyond 5 fields
	setClauses := []string{}
	args := []interface{}{}
	argIdx := 1

	if settings.ClientName != nil {
		setClauses = append(setClauses, "client_name = $"+itoa(argIdx))
		args = append(args, *settings.ClientName)
		argIdx++
	}
	if settings.BaseCurrency != nil {
		setClauses = append(setClauses, "base_currency = $"+itoa(argIdx))
		args = append(args, *settings.BaseCurrency)
		argIdx++
	}
	if settings.FiscalYearStartMonth != nil {
		setClauses = append(setClauses, "fiscal_year_start_month = $"+itoa(argIdx))
		args = append(args, *settings.FiscalYearStartMonth)
		argIdx++
	}
	if settings.AutoLinkConfidenceThreshold != nil {
		setClauses = append(setClauses, "auto_link_confidence_threshold = $"+itoa(argIdx))
		args = append(args, *settings.AutoLinkConfidenceThreshold)
		argIdx++
	}
	if settings.ReviewConfidenceFloor != nil {
		setClauses = append(setClauses, "review_confidence_floor = $"+itoa(argIdx))
		args = append(args, *settings.ReviewConfidenceFloor)
		argIdx++
	}

	if len(setClauses) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no settings provided"})
		return
	}

	args = append(args, bookID)
	query := "UPDATE client_books SET " + join(setClauses, ", ") + " WHERE id = $" + itoa(argIdx)
	_, err := db.Exec(r.Context(), query, args...)
	if err != nil {
		slog.Error("failed to update book settings", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to update settings"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"message": "settings updated"})
}

func (s *Service) HandleAssignStaff(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("bookId")
	if bookID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bookId is required"})
		return
	}

	// Only firm_admin can assign staff — enforced by RequireRole middleware at /v1/admin
	var req struct {
		UserID string `json:"user_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if req.UserID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "user_id is required"})
		return
	}

	_, err := s.db.Exec(r.Context(),
		`INSERT INTO user_book_assignments (user_id, client_book_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
		req.UserID, bookID)
	if err != nil {
		slog.Error("failed to assign staff", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to assign staff"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"message": "staff assigned"})
}

func (s *Service) HandleRemoveStaff(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("bookId")
	userID := r.PathValue("userId")
	if bookID == "" || userID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bookId and userId are required"})
		return
	}

	_, err := s.db.Exec(r.Context(),
		`DELETE FROM user_book_assignments WHERE user_id = $1 AND client_book_id = $2`,
		userID, bookID)
	if err != nil {
		slog.Error("failed to remove staff", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to remove staff"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"message": "staff removed"})
}

// HandleListStaff lists all users in the current firm (admin only)
func (s *Service) HandleListStaff(w http.ResponseWriter, r *http.Request) {
	firmID := middleware.GetFirmID(r.Context())
	if firmID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	rows, err := s.db.Query(r.Context(),
		"SELECT id::text, email, role FROM users WHERE firm_id = $1 ORDER BY created_at DESC", firmID)
	if err != nil {
		slog.Error("failed to list staff", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	defer rows.Close()

	var staff []map[string]string
	for rows.Next() {
		var id, email, role string
		if err := rows.Scan(&id, &email, &role); err != nil {
			continue
		}
		staff = append(staff, map[string]string{"id": id, "email": email, "role": role})
	}
	if staff == nil {
		staff = []map[string]string{}
	}
	writeJSON(w, http.StatusOK, staff)
}

func (s *Service) HandleGetFirmSettings(w http.ResponseWriter, r *http.Request) {
	firmID := middleware.GetFirmID(r.Context())
	if firmID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	var name, brandColor, reportFooter string
	err := s.db.QueryRow(r.Context(),
		"SELECT name, brand_primary_color, COALESCE(report_footer_text, '') FROM firms WHERE id = $1",
		firmID).Scan(&name, &brandColor, &reportFooter)
	if err != nil {
		slog.Error("failed to get firm settings", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"name": name,
		"brand_primary_color": brandColor,
		"report_footer_text":  reportFooter,
	})
}

func (s *Service) HandleUpdateFirmSettings(w http.ResponseWriter, r *http.Request) {
	firmID := middleware.GetFirmID(r.Context())
	if firmID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	var settings struct {
		Name               *string `json:"name"`
		BrandPrimaryColor  *string `json:"brand_primary_color"`
		ReportFooterText   *string `json:"report_footer_text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&settings); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	setClauses := []string{}
	args := []interface{}{}
	argIdx := 1
	if settings.Name != nil {
		setClauses = append(setClauses, "name = $"+itoa(argIdx))
		args = append(args, *settings.Name)
		argIdx++
	}
	if settings.BrandPrimaryColor != nil {
		setClauses = append(setClauses, "brand_primary_color = $"+itoa(argIdx))
		args = append(args, *settings.BrandPrimaryColor)
		argIdx++
	}
	if settings.ReportFooterText != nil {
		setClauses = append(setClauses, "report_footer_text = $"+itoa(argIdx))
		args = append(args, *settings.ReportFooterText)
		argIdx++
	}

	if len(setClauses) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no settings provided"})
		return
	}

	args = append(args, firmID)
	query := "UPDATE firms SET " + join(setClauses, ", ") + " WHERE id = $" + itoa(argIdx)
	_, err := s.db.Exec(r.Context(), query, args...)
	if err != nil {
		slog.Error("failed to update firm settings", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to update settings"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"message": "settings updated"})
}

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

func join(parts []string, sep string) string {
	result := ""
	for i, p := range parts {
		if i > 0 {
			result += sep
		}
		result += p
	}
	return result
}

func itoa(n int) string {
	return fmt.Sprintf("%d", n)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
