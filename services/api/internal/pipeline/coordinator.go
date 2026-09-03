package pipeline

// Coordinator — the missing bridge in the document pipeline (doc 12 §1).
//
//   document.uploaded ──> ingestion gRPC (parse OFX/CSV/PDF) ──> extracted_entities
//        ──> entity.extraction.requested ──> agent-runtime (link/classify)
//
// This is the piece that makes document.uploaded actually do something. Without it,
// uploads confirm but no entities ever reach the DB and no three-way reconciliation
// can run.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	ingestionpb "github.com/tanzeelgcuf/ai-auditor/services/api/genproto/ingestion"
	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/storage"
)

// ErrNoIngestion indicates the coordinator has no live ingestion connection.
var ErrNoIngestion = errors.New("no ingestion connection")

// maxDeliveryAttempts bounds JetStream redelivery for document.uploaded. After
// this many failed attempts the document is marked 'failed' rather than being
// retried forever; see the ConsumerConfig comment in Run.
const maxDeliveryAttempts = 5

type Coordinator struct {
	nc          *nats.Conn
	db          *pgxpool.Pool
	storage     *storage.Client
	ingestionURL string
	ingestion   ingestionpb.IngestionServiceClient
	js          jetstream.JetStream
}

func NewCoordinator(natsURL, ingestionURL string, db *pgxpool.Pool, st *storage.Client) (*Coordinator, error) {
	nc, err := nats.Connect(natsURL)
	if err != nil {
		return nil, err
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, err
	}

	conn, err := grpc.NewClient(ingestionURL, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		nc.Close()
		return nil, err
	}

	return &Coordinator{
		nc:           nc,
		db:           db,
		storage:      st,
		ingestionURL: ingestionURL,
		ingestion:    ingestionpb.NewIngestionServiceClient(conn),
		js:           js,
	}, nil
}

// Close releases the NATS + gRPC connections.
func (c *Coordinator) Close() {
	if c.nc != nil {
		c.nc.Close()
	}
}

// Run consumes document.uploaded events forever (blocking). Each event triggers
// ingestion gRPC, then writes the parsed entities, then requests extraction.
func (c *Coordinator) Run(ctx context.Context) error {
	attachCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cons, err := c.js.CreateOrUpdateConsumer(ctx, "DOCUMENTS", jetstream.ConsumerConfig{
		Durable:       "coordinator",
		AckPolicy:     jetstream.AckExplicitPolicy,
		DeliverPolicy: jetstream.DeliverAllPolicy,

		// FilterSubject is REQUIRED, not cosmetic. The DOCUMENTS stream carries
		// three subjects (pipeline.go:41) and this consumer previously received
		// all of them, discarding two with a bare Ack in the loop below. Now that
		// handleUploaded publishes document.processing.failed to that same
		// stream, an unfiltered consumer would be delivered its own failure
		// events — a self-feedback loop it then acks and drops.
		FilterSubject: "document.uploaded",

		// MaxDeliver is REQUIRED because the error paths below now Nak instead of
		// dropping. The default is -1 (unlimited): a document that fails
		// deterministically — a corrupt PDF that always fails the ingestion gRPC
		// — would be redelivered forever at full speed. Five attempts, then the
		// document is marked failed and the message is acked.
		MaxDeliver: maxDeliveryAttempts,

		// The default AckWait is 30s. OCR of a 25MB scanned PDF through the docTR
		// sidecar can exceed that, and an expired AckWait means the message is
		// redelivered while the first attempt is still running — duplicate
		// entities for one document. Two minutes is sized for the slow path.
		AckWait: 2 * time.Minute,
	})
	_ = attachCtx
	if err != nil {
		// Non-fatal: a consumer may already exist; try to load it.
		cons, err = c.js.Consumer(ctx, "DOCUMENTS", "coordinator")
		if err != nil {
			return err
		}
	}

	for {
		msg, err := cons.Next()
		if err != nil {
			slog.Warn("coordinator consumer error", "error", err)
			time.Sleep(1 * time.Second)
			continue
		}
		if subj := msg.Subject(); subj == "document.uploaded" {
			c.handleUploaded(ctx, msg)
		} else {
			msg.Ack() // ignore other subjects on the stream
		}
	}
}

// fail handles a processing error for one document.
//
// Before this existed, every error path in handleUploaded was `slog.Error(...)
// + return` under a `defer msg.Ack()`. That combination ACKNOWLEDGED AND
// DISCARDED the event: no retry, no dead letter, and — worse — no state change,
// so source_documents.ocr_status stayed 'pending' forever while the UI showed a
// document that was quietly never going to be processed. For a reconciliation
// product that is silent data loss: the book reconciles against whatever
// happened to load, and nothing says a document is missing.
//
// Retry vs. give up is decided by the delivery count, not by guessing which
// errors are transient: a MinIO blip and a corrupt file look identical here, and
// the cheap correct policy is to retry a bounded number of times and then record
// the failure where a human can see it.
func (c *Coordinator) fail(ctx context.Context, msg jetstream.Msg, docID, stage string, cause error) {
	attempt := uint64(1)
	if md, err := msg.Metadata(); err == nil {
		attempt = md.NumDelivered
	}

	if attempt < maxDeliveryAttempts {
		slog.Warn("coordinator: retrying document",
			"doc", docID, "stage", stage, "attempt", attempt,
			"max", maxDeliveryAttempts, "error", cause)
		// Nak asks JetStream to redeliver. AckWait/MaxDeliver on the consumer
		// bound how long and how often.
		if err := msg.Nak(); err != nil {
			slog.Error("coordinator: nak failed", "doc", docID, "error", err)
		}
		return
	}

	slog.Error("coordinator: document failed permanently",
		"doc", docID, "stage", stage, "attempts", attempt, "error", cause)

	// The status write is what makes the failure visible to the API and the UI.
	// It is best-effort by necessity — if the database is the thing that is
	// broken, there is nowhere left to record the problem — but unlike the
	// previous silent return, a failure to record it is itself logged.
	if docID != "" {
		if _, err := c.db.Exec(ctx,
			`UPDATE source_documents SET ocr_status = 'failed' WHERE id = $1`, docID); err != nil {
			slog.Error("coordinator: could not mark document failed",
				"doc", docID, "error", err)
		}
	}

	// document.processing.failed is declared on the DOCUMENTS stream
	// (pipeline.go:41) and, until now, had NO PRODUCER anywhere in the
	// repository — the failure signal was designed and never wired.
	if payload, err := json.Marshal(map[string]string{
		"document_id": docID, "stage": stage, "error": cause.Error(),
	}); err == nil {
		if _, err := c.js.Publish(ctx, "document.processing.failed", payload); err != nil {
			slog.Error("coordinator: could not publish failure event", "doc", docID, "error", err)
		}
	}

	// Ack only now: the event is terminal and must not come back.
	if err := msg.Ack(); err != nil {
		slog.Error("coordinator: ack failed", "doc", docID, "error", err)
	}
}

func (c *Coordinator) handleUploaded(ctx context.Context, msg jetstream.Msg) {
	var ev struct {
		DocumentID   string `json:"document_id"`
		ClientBookID string `json:"client_book_id"`
		StorageKey   string `json:"storage_key"`
		DocType      string `json:"doc_type"`
	}
	if err := json.Unmarshal(msg.Data(), &ev); err != nil || ev.DocumentID == "" {
		// A malformed payload will never become well-formed; retrying is
		// pointless, so this one path acks immediately rather than going through
		// fail(). There is no document_id to mark.
		slog.Error("coordinator: bad document.uploaded payload", "data", string(msg.Data()))
		_ = msg.Ack()
		return
	}

	slog.Info("coordinator: processing document", "doc", ev.DocumentID, "type", ev.DocType)

	// Stream the file from MinIO — ingestion needs the bytes to parse.
	raw, err := c.storage.StreamObject(ctx, ev.StorageKey)
	if err != nil {
		// This is the exact error the discarded-bytes bug produced: HandleUpload
		// wrote a source_documents row and published this event without ever
		// writing the object, so StreamObject returned NoSuchKey on a key that
		// had never existed. That is fixed at the source in
		// internal/documents.HandleUpload; this path remains for real storage
		// faults.
		c.fail(ctx, msg, ev.DocumentID, "stream_object", err)
		return
	}

	// Fetch the book's CSV column mapping (doc 08 §1) so structured formats parse
	// with the correct header mapping, not an empty one. The mapping is chosen
	// by matching the file's actual header row against each stored mapping's
	// source columns — a firm may hold multiple exports (QBO, Xero, custom)
	// needing different maps (Prompt C: the most-recent-mapping heuristic sent
	// a QBO export through the stress-set map, dropping counterparty/account).
	columnMap := c.fetchColumnMap(ctx, ev.ClientBookID, raw)

	// Call ingestion gRPC: it parses the bytes into structured entities.
	resp, err := c.ingestion.ProcessDocument(ctx, &ingestionpb.ProcessDocumentRequest{
		DocumentId:    ev.DocumentID,
		ClientBookId:  ev.ClientBookID,
		StorageKey:    ev.StorageKey,
		DocType:       ev.DocType,
		ColumnMap:     columnMap,
	})
	if err != nil {
		c.fail(ctx, msg, ev.DocumentID, "ingestion_grpc", err)
		return
	}

	// Write parsed entities to extracted_entities.
	if err := c.persistEntities(ctx, ev.ClientBookID, ev.DocumentID, resp.Entities); err != nil {
		c.fail(ctx, msg, ev.DocumentID, "persist_entities", err)
		return
	}

	// Mark the source document done.
	_, _ = c.db.Exec(ctx,
		"UPDATE source_documents SET ocr_status = 'done' WHERE id = $1", ev.DocumentID)

	// Request agent-runtime to extract this document's entities (per-doc — a
	// single LLM prompt must not span the whole book).
	req := map[string]string{"client_book_id": ev.ClientBookID, "batch_id": ev.DocumentID}
	if payload, err := json.Marshal(req); err == nil {
		_, _ = c.js.Publish(ctx, "entity.extraction.requested", payload)
	}

	// Then trigger a BOOK-WIDE link pass: 3-way reconciliation needs entities
	// from the invoice + bank + GL documents together, which per-doc extraction
	// can't produce. The link handler fetches the whole book's pending set.
	if payload, err := json.Marshal(map[string]string{"client_book_id": ev.ClientBookID}); err == nil {
		_, _ = c.js.Publish(ctx, "link.requested", payload)
	}

	// Ack LAST, and only on the success path. The previous `defer msg.Ack()` at
	// the top of this function acked before any of the work was attempted, which
	// is what made every error path a silent drop.
	if err := msg.Ack(); err != nil {
		slog.Error("coordinator: ack failed after successful processing",
			"doc", ev.DocumentID, "error", err)
	}
}

// fetchColumnMap selects the csv_column_mapping whose source columns best match
// the file's header row. Falls back to the most recent mapping when no header
// match (e.g. PDF/OFX, or an unknown export).
func (c *Coordinator) fetchColumnMap(ctx context.Context, bookID string, fileBytes []byte) map[string]string {
	out := map[string]string{}

	// Extract the header row from CSV-like content.
	var header []string
	if i := bytes.IndexByte(fileBytes, '\n'); i > 0 {
		first := string(fileBytes[:i])
		header = strings.Split(first, ",")
		for j := range header {
			header[j] = strings.TrimSpace(strings.Trim(header[j], `"'`))
		}
	}

	rows, err := c.db.Query(ctx,
		`SELECT column_map FROM csv_column_mappings WHERE client_book_id = $1 ORDER BY created_at DESC`,
		bookID)
	if err != nil {
		return out
	}
	defer rows.Close()

	type scored struct {
		colmap map[string]string
		score  int
	}
	var best scored
	var latest map[string]string
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			continue
		}
		var m map[string]string
		if err := json.Unmarshal(raw, &m); err != nil {
			continue
		}
		latest = m
		if len(header) == 0 {
			continue
		}
		score := mappingHeaderScore(m, header)
		if score > best.score {
			best = scored{colmap: m, score: score}
		}
	}
	if best.score > 0 {
		return best.colmap
	}
	if latest != nil {
		return latest
	}
	return out
}

// mappingHeaderScore counts how many of a mapping's source column names appear
// in the file's header row (case-insensitive). The mapping with the most hits
// is the one the file was exported from.
func mappingHeaderScore(m map[string]string, header []string) int {
	score := 0
	for _, src := range m {
		for _, h := range header {
			if strings.EqualFold(src, h) {
				score++
				break
			}
		}
	}
	return score
}

// nullableDate converts a "YYYY-MM-DD" string (or empty) to *time.Time for the
// DATE column, or nil if unparseable.
func nullableDate(s string) *time.Time {
	if s == "" {
		return nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return &t
	}
	return nil
}

// bboxJSON renders an entity's OCR geometry as the JSON stored in the bbox
// column, or "{}" when absent. The column was previously hardcoded '{}' — the
// sidecar produces real coordinates but they were dropped here, so citations
// pointed at nothing (traceability gap, Round 7).
func bboxJSON(e *ingestionpb.ExtractedEntity) string {
	if e.Bbox == nil {
		return "{}"
	}
	if b, err := json.Marshal(e.Bbox); err == nil {
		return string(b)
	}
	return "{}"
}

func (c *Coordinator) persistEntities(ctx context.Context, bookID, docID string, ents []*ingestionpb.ExtractedEntity) error {
	if len(ents) == 0 {
		slog.Info("coordinator: no entities parsed", "doc", docID)
		return nil
	}
	for _, e := range ents {
		txnDate := nullableDate(e.TransactionDate)
		_, err := c.db.Exec(ctx,
			`INSERT INTO extracted_entities
				(client_book_id, source_document_id, entity_type, amount_cents, transaction_date,
				 counterparty, description, gl_account_code, transaction_ref, page_number, bbox, extraction_confidence, source_format)
			 VALUES ($1, $2, $3, $4, $5, NULLIF($6,''), NULLIF($7,''), NULLIF($8,''), NULLIF($9,''),
			 	$10, $13, $11, $12)`,
			bookID, docID, e.EntityType, e.AmountCents, txnDate,
			e.Counterparty, e.Description, e.GlAccountCode, e.TransactionRef,
			e.PageNumber, e.Confidence, e.SourceFormat, bboxJSON(e))
		if err != nil {
			return err
		}
	}
	slog.Info("coordinator: persisted entities", "doc", docID, "count", len(ents))
	return nil
}