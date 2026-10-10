// The IP retention sweep. Until 2026-10-01 access_log rows lived forever —
// no TTL existed, which is the SOC2_READINESS.md "Audit-log IP retention
// policy" white row ("no TTL exists, so today's answer is forever"). The
// window is AUDIT_LOG_RETENTION_DAYS (default 90); the sweep deletes rows
// older than it. Runs on the server root context from main.go, so its ctx
// cancelling IS graceful shutdown — the same pattern as notify.Run, and the
// same reason it is allowlisted in check_cancellable_audit_writes.py's
// reasoning for background sweeps.
package auditlog

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const DefaultInterval = 6 * time.Hour

// RetentionDays reads AUDIT_LOG_RETENTION_DAYS. Unset, empty, invalid, or
// non-positive returns 90: long enough for an audit cycle, short enough that
// the table is bounded. A bad value must not crash the sweeper at boot.
func RetentionDays() int {
	v := os.Getenv("AUDIT_LOG_RETENTION_DAYS")
	if v == "" {
		return 90
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 90
	}
	return n
}

// RunRetention loops forever, sweeping expired access_log rows every
// interval. Returns when ctx is cancelled.
func RunRetention(ctx context.Context, db *pgxpool.Pool, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		sweepOnce(ctx, db)
		select {
		case <-ctx.Done():
			slog.Info("audit-log retention sweep stopped")
			return
		case <-t.C:
		}
	}
}

// SweepOnce exposes a single sweep for tests and one-off invocations.
func SweepOnce(ctx context.Context, db *pgxpool.Pool) (int64, error) {
	days := RetentionDays()
	cutoff := time.Now().AddDate(0, 0, -days)
	tag, err := db.Exec(ctx, `DELETE FROM access_log WHERE occurred_at < $1`, cutoff)
	if err != nil {
		return 0, err
	}
	if tag.RowsAffected() > 0 {
		slog.Info("audit-log retention sweep", "deleted", tag.RowsAffected(), "retention_days", days)
	}
	return tag.RowsAffected(), nil
}

func sweepOnce(ctx context.Context, db *pgxpool.Pool) {
	if _, err := SweepOnce(ctx, db); err != nil {
		slog.Error("audit-log retention sweep failed", "error", err)
	}
}
