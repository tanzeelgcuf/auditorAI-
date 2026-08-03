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
	cons, err := w.js.CreateOrUpdateConsumer(ctx, "VERIFY", jetstream.ConsumerConfig{
		Durable:       "verify-worker",
		AckPolicy:     jetstream.AckExplicitPolicy,
		DeliverPolicy: jetstream.DeliverAllPolicy,
	})
	if err != nil {
		cons, err = w.js.Consumer(ctx, "VERIFY", "verify-worker")
		if err != nil {
			return err
		}
	}

	for {
		msg, err := cons.Next()
		if err != nil {
			slog.Warn("verify worker consumer error", "error", err)
			time.Sleep(1 * time.Second)
			continue
		}
		if msg.Subject() == "verification.requested" {
			w.handleVerification(ctx, msg)
		} else {
			msg.Ack()
		}
	}
}

func (w *VerifyWorker) handleVerification(ctx context.Context, msg jetstream.Msg) {
	defer msg.Ack()

	var ev struct {
		GroupID      string `json:"group_id"`
		ClientBookID string `json:"client_book_id"`
	}
	if err := json.Unmarshal(msg.Data(), &ev); err != nil || ev.GroupID == "" {
		slog.Error("verify worker: bad payload", "data", string(msg.Data()))
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
		slog.Error("verify worker: load group failed", "group", ev.GroupID, "error", err)
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
		slog.Error("verify worker: gRPC evaluate failed", "group", ev.GroupID, "error", err)
		return
	}
	if len(res.Results) == 0 {
		slog.Error("verify worker: no result returned", "group", ev.GroupID)
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
		slog.Error("verify worker: insert finding failed", "group", ev.GroupID, "error", err)
		return
	}

	slog.Info("finding created",
		"group", ev.GroupID, "variance_cents", r.VarianceCents,
		"severity", r.Severity, "exceeds", r.ExceedsTolerance)
}
