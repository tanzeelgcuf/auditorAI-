// Package notify runs the proactive reminder job (doc 10 §7): every interval
// it finds reconciliation periods sitting in pending_close whose open document
// requests are older than 5 days, bumps the reminder counter, and logs each
// reminder. Requests that accumulate 3+ reminders surface as "stale" on the
// firm dashboard (derived, no extra table).
package notify

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const DefaultInterval = 6 * time.Hour

// reminderWindow is the age an open document request must reach before a
// reminder is sent. Exported so tests can inject a short window.
var reminderWindow = 5 * 24 * time.Hour

// Run loops forever, running sendReminders immediately then every interval.
// Returns when ctx is cancelled. Errors are logged and the loop continues —
// a DB hiccup must never kill the reminder job.
func Run(ctx context.Context, db *pgxpool.Pool, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		runOnce(ctx, db)
		select {
		case <-ctx.Done():
			slog.Info("reminder job stopped")
			return
		case <-t.C:
		}
	}
}

// RunOnce exposes a single sweep for tests and one-off invocations.
func RunOnce(ctx context.Context, db *pgxpool.Pool) {
	runOnce(ctx, db)
}

// runOnce finds candidate document requests and bumps their reminder count.
// The whole sweep is logged per request; a failure in one book/request does not
// abort the sweep.
func runOnce(ctx context.Context, db *pgxpool.Pool) {
	rows, err := db.Query(ctx, `
		SELECT dr.id::text, dr.client_book_id::text, p.id::text, p.period_start, p.period_end,
		       dr.requested_doc_type, dr.reminder_sent_count
		FROM document_requests dr
		JOIN reconciliation_periods p ON p.id = dr.reconciliation_period_id
		WHERE dr.status = 'pending'
		  AND dr.reminder_sent_count < 3
		  AND p.status = 'pending_close'
		  AND dr.requested_at <= now() - make_interval(secs => $1::float8)`,
		reminderWindow.Seconds())
	if err != nil {
		slog.Error("reminder sweep: query failed", "error", err)
		return
	}
	defer rows.Close()

	var sent int
	for rows.Next() {
		var reqID, bookID, periodID, periodStart, periodEnd, docType string
		var count int
		if err := rows.Scan(&reqID, &bookID, &periodID, &periodStart, &periodEnd, &docType, &count); err != nil {
			slog.Error("reminder sweep: scan failed", "error", err)
			continue
		}
		if _, err := db.Exec(ctx,
			`UPDATE document_requests SET reminder_sent_count = reminder_sent_count + 1 WHERE id = $1`,
			reqID); err != nil {
			slog.Error("reminder sweep: increment failed", "request_id", reqID, "error", err)
			continue
		}
		sent++
		slog.Info("document request reminder sent",
			"request_id", reqID, "book_id", bookID, "period_id", periodID,
			"period_start", periodStart, "period_end", periodEnd,
			"doc_type", docType, "reminder_count", count+1)
	}
	if err := rows.Err(); err != nil {
		slog.Error("reminder sweep: rows error", "error", err)
	}
	if sent > 0 {
		slog.Info("reminder sweep complete", "reminders_sent", sent)
	}
}
