package middleware

// InternalAuth authenticates internal service-to-service calls (agent-runtime
// -> API MCP tools) with a shared secret instead of a user JWT (doc 05 §3).
//
// The request carries client_book_id in its JSON body; we resolve its firm and
// set a firm_admin context so RLSInjector grants access to that book. Without
// this, agent-runtime's MCP calls would 401 (no user token) or cross-tenant
// (no firm scope) — the exact wiring gap Prompt A targets.

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
)

// InternalAuth validates X-Internal-Key against API_INTERNAL_KEY. On success it
// resolves the client_book_id -> firm from the JSON body and sets the firm_admin
// context needed by RLSInjector. On mismatch it 401s (does not fall through to
// the user Authenticator).
func InternalAuth(db *pgxpool.Pool) func(http.Handler) http.Handler {
	expected := os.Getenv("API_INTERNAL_KEY")
	if expected == "" {
		slog.Warn("API_INTERNAL_KEY not set — internal MCP auth disabled")
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Header read only; body is parsed below for client_book_id.
			got := r.Header.Get("X-Internal-Key")
			if got == "" {
				writeProblem(w, r, "https://ai-auditor.dev/errors/unauthorized", "Unauthorized", http.StatusUnauthorized, "Missing X-Internal-Key")
				return
			}
			if expected == "" || subtle.ConstantTimeCompare([]byte(got), []byte(expected)) != 1 {
				writeProblem(w, r, "https://ai-auditor.dev/errors/unauthorized", "Unauthorized", http.StatusUnauthorized, "Invalid internal key")
				return
			}

			// Resolve the book to scope the firm. Most MCP calls carry
			// client_book_id; create_entity_link instead carries entity IDs
			// (invoice_ids/bank_ids/gl_ids) and derives the book from the first
			// entity. Support both. Read the whole body, parse, restore for the
			// handler.
			var body struct {
				ClientBookID string   `json:"client_book_id"`
				InvoiceIDs   []string `json:"invoice_ids"`
				BankIDs      []string `json:"bank_ids"`
				GLIDs        []string `json:"gl_ids"`
			}
			if r.Body != nil {
				raw, _ := io.ReadAll(r.Body)
				r.Body = io.NopCloser(bytes.NewReader(raw)) // restore for handler
				if len(raw) > 0 && len(raw) < 1<<20 {
					_ = json.Unmarshal(raw, &body)
				}
			}

			bookID := body.ClientBookID
			if bookID == "" {
				// Fall back to the first entity ID present (create_entity_link).
				for _, id := range append(append(body.InvoiceIDs, body.BankIDs...), body.GLIDs...) {
					if id != "" {
						var derived string
						err := db.QueryRow(r.Context(),
							`SELECT client_book_id::text FROM extracted_entities WHERE id = $1`, id).Scan(&derived)
						if err == nil {
							bookID = derived
							break
						}
					}
				}
			}
			if bookID == "" {
				writeProblem(w, r, "https://ai-auditor.dev/errors/bad-request", "Bad Request", http.StatusBadRequest, "could not resolve book for internal auth")
				return
			}

			var firmID string
			err := db.QueryRow(r.Context(),
				`SELECT b.id::text, b.firm_id::text FROM client_books b WHERE b.id = $1`,
				bookID).Scan(&bookID, &firmID)
			if err != nil {
				writeProblem(w, r, "https://ai-auditor.dev/errors/not-found", "Not Found", http.StatusNotFound, "book not found")
				return
			}

			// firm_admin context + RLS-wired connection. Internal MCP handlers
			// query through the same tenant-scoped path as user requests; without
			// the RLS GUCs on a dedicated conn, the book-isolation policy blocks
			// everything (Prompt A: get_pending_entities returned empty despite
			// rows in the DB).
			conn, err := db.Acquire(r.Context())
			if err != nil {
				writeProblem(w, r, "https://ai-auditor.dev/errors/internal", "Internal Error", http.StatusInternalServerError, "failed to acquire db conn")
				return
			}
			if _, err = conn.Exec(r.Context(), "SELECT set_config('app.current_firm', $1, false)", firmID); err != nil {
				conn.Release()
				writeProblem(w, r, "https://ai-auditor.dev/errors/internal", "Internal Error", http.StatusInternalServerError, "failed to set firm context")
				return
			}
			if _, err = conn.Exec(r.Context(), "SELECT set_config('app.assigned_books', $1, false)", bookID); err != nil {
				conn.Release()
				writeProblem(w, r, "https://ai-auditor.dev/errors/internal", "Internal Error", http.StatusInternalServerError, "failed to set book context")
				return
			}

			ctx := context.WithValue(r.Context(), FirmIDKey, firmID)
			ctx = context.WithValue(ctx, UserIDKey, "internal:"+firmID)
			ctx = context.WithValue(ctx, RoleKey, "firm_admin")
			ctx = context.WithValue(ctx, AssignedBooksKey, []string{bookID})
			ctx = context.WithValue(ctx, connKey, conn)
			slog.Info("internal auth ok", "book", bookID, "firm", firmID)
			next.ServeHTTP(w, r.WithContext(ctx))
			_, _ = conn.Exec(r.Context(), "RESET app.current_firm, app.assigned_books")
			conn.Release()
		})
	}
}
