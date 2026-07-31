package middleware

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
)

// RecordAccess writes one row to access_log for a sensitive action.
// clientBookID may be empty for actions that are not book-scoped.
// resourceID may be empty when the action is not about a single resource.
//
// The write goes through the RLS-wired request connection when available so
// the tenant GUCs (app.current_firm / app.assigned_books) are set; access_log
// RLS allows inserting when client_book_id is NULL or in assigned_books.
func RecordAccess(ctx context.Context, db *pgxpool.Pool, userID, clientBookID, action, resourceID string) {
	conn := GetConn(ctx)
	var err error
	if conn != nil {
		_, err = conn.Exec(ctx,
			`INSERT INTO access_log (user_id, client_book_id, action, resource_id)
			 VALUES ($1, NULLIF($2, '')::uuid, $3, NULLIF($4, '')::uuid)`,
			userID, clientBookID, action, resourceID)
	} else {
		_, err = db.Exec(ctx,
			`INSERT INTO access_log (user_id, client_book_id, action, resource_id)
			 VALUES ($1, NULLIF($2, '')::uuid, $3, NULLIF($4, '')::uuid)`,
			userID, clientBookID, action, resourceID)
	}
	if err != nil {
		// Audit logging must never break the request path — degrade to a log line.
		slog.Warn("failed to record access log", "action", action, "resource_id", resourceID, "error", err)
	}
}
