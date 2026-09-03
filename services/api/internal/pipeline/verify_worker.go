package pipeline

// VerifyWorker — the verification.requested consumer (Prompt 3).
//
// The pipeline published verification.requested but nothing consumed it, so
// linked groups never produced audit_findings. This worker subscribes to
// verification.requested, loads each group's three-leg totals from the DB,
// calls the Rust verification gRPC service (BatchEvaluate), and writes a
// finding per group. Deterministic math stays in Rust — this only moves data.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	verificationpb "github.com/tanzeelgcuf/ai-auditor/services/api/genproto/verification"
)

// VerifyWorker consumes verification.requested events and writes findings.
type VerifyWorker struct {
	nc           *nats.Conn
	js           jetstream.JetStream
	db           *pgxpool.Pool
	verification verificationpb.VerificationServiceClient
}

// NewVerifyWorker dials the verification gRPC service and NATS.
func NewVerifyWorker(natsURL, verificationURL string, db *pgxpool.Pool) (*VerifyWorker, error) {
	nc, err := nats.Connect(natsURL)
	if err != nil {
		return nil, err
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, err
	}
	conn, err := grpc.NewClient(verificationURL, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		nc.Close()
		return nil, err
	}
	return &VerifyWorker{
		nc:           nc,
		js:           js,
		db:           db,
		verification: verificationpb.NewVerificationServiceClient(conn),
	}, nil
}

// Close releases NATS + gRPC connections.
func (w *VerifyWorker) Close() {
	if w.nc != nil {
		w.nc.Close()
	}
}

// Run consumes verification.requested forever (blocking).
func (w *VerifyWorker) Run(ctx context.Context) error {
	// Bounded like the coordinator's: both calls below are NATS round trips, and
	// an unbounded wait here blocks the worker forever with no log line while
	// linked groups pile up unverified.
	attachCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cons, err := w.js.CreateOrUpdateConsumer(attachCtx, "VERIFY", jetstream.ConsumerConfig{
		Durable:       "verify-worker",
		AckPolicy:     jetstream.AckExplicitPolicy,
		DeliverPolicy: jetstream.DeliverAllPolicy,

		// Same three settings, for the same reasons, as the coordinator's consumer
		// — see coordinator.go. They are required here now that the error paths
		// Nak instead of dropping: without MaxDeliver a group that fails
		// deterministically is redelivered forever, and the default 30s AckWait is
		// shorter than a BatchEvaluate round trip against a cold verification
		// service, which would redeliver while the first attempt is still running.
		FilterSubject: "verification.requested",
		MaxDeliver:    maxDeliveryAttempts,
		AckWait:       2 * time.Minute,
	})
	if err != nil {
		slog.Warn("verify worker: consumer create/update failed, binding to the existing "+
			"durable — its config may predate FilterSubject/MaxDeliver/AckWait",
			"error", err)
		cons, err = w.js.Consumer(attachCtx, "VERIFY", "verify-worker")
		if err != nil {
			return err
		}
	}

	for {
		if err := ctx.Err(); err != nil {
			slog.Info("verify worker stopping", "reason", err)
			return err
		}
		msg, err := cons.Next()
		if err != nil {
			slog.Warn("verify worker consumer error", "error", err)
			time.Sleep(1 * time.Second)
			continue
		}
		if msg.Subject() == "verification.requested" {
			w.handleVerification(ctx, msg)
		} else {
			_ = msg.Ack()
		}
	}
}

// fail handles a processing error for one reconciliation group.
//
// This is the same bug class as coordinator.fail, found in this file by sweeping
// for it after fixing the first instance. handleVerification opened with
// `defer msg.Ack()` and every error path was `slog.Error(...) + return`, so the
// event was acknowledged and discarded — and the consequence here is worse than a
// stuck document. A group was linked, verification.requested was published, the
// worker failed (DB blip, verification service down, empty gRPC result), the
// event was dropped, and NO FINDING WAS EVER WRITTEN. The group then reads as
// linked with nothing flagged against it, which is indistinguishable from
// "reconciled cleanly". An audit product that loses findings silently reports the
// wrong answer with full confidence.
//
// On permanent failure the group is moved to 'needs_review' so a human sees it.
// That deliberately reuses an existing status value rather than adding
// 'verification_failed' to the CHECK constraint: a new value means touching the
// CHECK in infra/init.sql, the review-queue status filter (review.go:71) and the
// TS union in apps/web/lib/hooks.ts:34, and the conflation it costs is that
// "the matcher was unsure" and "verification could not run" land in the same
// queue. Worth separating later; not worth blocking the correctness fix on now.
func (w *VerifyWorker) fail(ctx context.Context, msg jetstream.Msg, groupID, stage string, cause error) {
	attempt := uint64(1)
	if md, err := msg.Metadata(); err == nil {
		attempt = md.NumDelivered
	}

	if attempt < maxDeliveryAttempts {
		slog.Warn("verify worker: retrying group",
			"group", groupID, "stage", stage, "attempt", attempt,
			"max", maxDeliveryAttempts, "error", cause)
		if err := msg.Nak(); err != nil {
			slog.Error("verify worker: nak failed", "group", groupID, "error", err)
		}
		return
	}

	slog.Error("verify worker: group failed permanently — no finding will be written",
		"group", groupID, "stage", stage, "attempts", attempt, "error", cause)

	if groupID != "" {
		if _, err := w.db.Exec(ctx,
			`UPDATE reconciliation_groups SET status = 'needs_review'
			  WHERE id = $1 AND status = 'auto_linked'`, groupID); err != nil {
			slog.Error("verify worker: could not flag group for review",
				"group", groupID, "error", err)
		}
	}

	if err := msg.Ack(); err != nil {
		slog.Error("verify worker: ack failed", "group", groupID, "error", err)
	}
}

func (w *VerifyWorker) handleVerification(ctx context.Context, msg jetstream.Msg) {
	var ev struct {
		GroupID      string `json:"group_id"`
		ClientBookID string `json:"client_book_id"`
	}
	if err := json.Unmarshal(msg.Data(), &ev); err != nil || ev.GroupID == "" {
		// Malformed will not become well-formed, and there is no group id to flag.
		slog.Error("verify worker: bad payload", "data", string(msg.Data()))
		_ = msg.Ack()
		return
	}

	// Load the group's three-leg totals, presence, and tolerance. A leg is
	// present iff it has ≥1 member — absent legs must be excluded from variance
	// or a balanced 2-leg group (e.g. deposit: bank+GL only) flags as a false
	// positive (Prompt 3: group 2cbd60f4 was $6,200 variance on equal legs).
	var invTotal, bankTotal, glTotal int64
	var hasInv, hasBank, hasGl bool
	var tolerance int32
	err := w.db.QueryRow(ctx,
		`SELECT
			COALESCE(SUM(CASE WHEN m.role='invoice' THEN e.amount_cents ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN m.role='bank' THEN e.amount_cents ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN m.role='gl' THEN e.amount_cents ELSE 0 END), 0),
			COALESCE(BOOL_OR(m.role='invoice'), false),
			COALESCE(BOOL_OR(m.role='bank'), false),
			COALESCE(BOOL_OR(m.role='gl'), false),
			cb.reconciliation_tolerance_cents
		 FROM reconciliation_groups g
		 JOIN reconciliation_group_members m ON m.reconciliation_group_id = g.id
		 JOIN extracted_entities e ON e.id = m.extracted_entity_id
		 JOIN client_books cb ON cb.id = g.client_book_id
		 WHERE g.id = $1
		 GROUP BY cb.reconciliation_tolerance_cents`, ev.GroupID).Scan(
		&invTotal, &bankTotal, &glTotal, &hasInv, &hasBank, &hasGl, &tolerance)
	if err != nil {
		// Includes pgx.ErrNoRows, which is not necessarily permanent: the group and
		// its members are written by a different path, so an event that arrives
		// before those rows are visible must be retried rather than dropped.
		w.fail(ctx, msg, ev.GroupID, "load_group", err)
		return
	}

	// Ask the Rust service for the verdict (deterministic math — not here).
	res, err := w.verification.BatchEvaluate(ctx, &verificationpb.BatchReconciliationRequest{
		ClientBookId: ev.ClientBookID,
		Groups: []*verificationpb.GroupReconciliation{{
			GroupId:           ev.GroupID,
			InvoiceTotalCents: invTotal,
			BankTotalCents:    bankTotal,
			GlTotalCents:      glTotal,
			ToleranceCents:    tolerance,
			HasInvoice:        hasInv,
			HasBank:           hasBank,
			HasGl:             hasGl,
		}},
	})
	if err != nil {
		w.fail(ctx, msg, ev.GroupID, "grpc_evaluate", err)
		return
	}
	if len(res.Results) == 0 {
		// An empty result for a request that carried exactly one group means the
		// verification service and this caller disagree about the contract. Retried
		// like any other failure, then surfaced — the alternative, dropping it,
		// leaves the group looking reconciled.
		w.fail(ctx, msg, ev.GroupID, "empty_result",
			errors.New("verification returned no results for a single-group request"))
		return
	}
	r := res.Results[0]

	// Write the finding (idempotent: skip if a finding already exists for this
	// group — re-verification of an existing group is a review-cycle concern,
	// out of v1 scope).
	_, err = w.db.Exec(ctx,
		`INSERT INTO audit_findings
			(client_book_id, reconciliation_group_id, rule_id, rule_version,
			 calculated_variance_cents, tolerance_cents, exceeds_tolerance,
			 calculation_formula, severity, status)
		 SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9,'open'
		 WHERE NOT EXISTS (
		   SELECT 1 FROM audit_findings WHERE reconciliation_group_id = $2)`,
		ev.ClientBookID, ev.GroupID, r.RuleId, r.RuleVersion,
		r.VarianceCents, tolerance, r.ExceedsTolerance,
		r.CalculationFormula, r.Severity)
	if err != nil {
		w.fail(ctx, msg, ev.GroupID, "insert_finding", err)
		return
	}

	slog.Info("finding created",
		"group", ev.GroupID, "variance_cents", r.VarianceCents,
		"severity", r.Severity, "exceeds", r.ExceedsTolerance)

	// Ack last, success path only. The INSERT is guarded by NOT EXISTS on
	// reconciliation_group_id, so a redelivery after a crash between the write and
	// this ack is a no-op rather than a duplicate finding.
	if err := msg.Ack(); err != nil {
		slog.Error("verify worker: ack failed after successful processing",
			"group", ev.GroupID, "error", err)
	}
}
