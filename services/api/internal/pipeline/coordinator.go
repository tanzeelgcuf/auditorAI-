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

	ingestionpb "github.com/tanzeelgcuf/ai-auditor/services/api/genproto/ingestion"
	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/storage"
)

// ErrNoIngestion indicates the coordinator has no live ingestion connection.
var ErrNoIngestion = errors.New("no ingestion connection")

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

func (c *Coordinator) handleUploaded(ctx context.Context, msg jetstream.Msg) {
	defer msg.Ack()

	var ev struct {
		DocumentID   string `json:"document_id"`
		ClientBookID string `json:"client_book_id"`
		StorageKey   string `json:"storage_key"`
		DocType      string `json:"doc_type"`
	}
	if err := json.Unmarshal(msg.Data(), &ev); err != nil || ev.DocumentID == "" {
		slog.Error("coordinator: bad document.uploaded payload", "data", string(msg.Data()))
		return
	}

	slog.Info("coordinator: processing document", "doc", ev.DocumentID, "type", ev.DocType)

	// Stream the file from MinIO — ingestion needs the bytes to parse.
	raw, err := c.storage.StreamObject(ctx, ev.StorageKey)
	if err != nil {
		slog.Error("coordinator: stream object failed", "doc", ev.DocumentID, "error", err)
		return
	}

	// Fetch the book's CSV column mapping (doc 08 §1) so structured formats parse
	// with the correct header mapping, not an empty one.
	columnMap := c.fetchColumnMap(ctx, ev.ClientBookID)

	// Call ingestion gRPC: it parses the bytes into structured entities.
	resp, err := c.ingestion.ProcessDocument(ctx, &ingestionpb.ProcessDocumentRequest{
		DocumentId:    ev.DocumentID,
		ClientBookId:  ev.ClientBookID,
		StorageKey:    ev.StorageKey,
		DocType:       ev.DocType,
		ColumnMap:     columnMap,
	})
	if err != nil {
		slog.Error("coordinator: ingestion gRPC failed", "doc", ev.DocumentID, "error", err)
		return
	}

	// Write parsed entities to extracted_entities.
	if err := c.persistEntities(ctx, ev.ClientBookID, ev.DocumentID, resp.Entities); err != nil {
		slog.Error("coordinator: persist entities failed", "doc", ev.DocumentID, "error", err)
		return
	}

	// Mark the source document done.
	_, _ = c.db.Exec(ctx,
		"UPDATE source_documents SET ocr_status = 'done' WHERE id = $1", ev.DocumentID)

	// Request agent-runtime to extract/link/classify this batch.
	req := map[string]string{"client_book_id": ev.ClientBookID, "batch_id": ev.DocumentID}
	if payload, err := json.Marshal(req); err == nil {
		_, _ = c.js.Publish(ctx, "entity.extraction.requested", payload)
	}

	_ = raw
}

// fetchColumnMap loads the most recent csv_column_mappings for a book (doc 08 §1).
func (c *Coordinator) fetchColumnMap(ctx context.Context, bookID string) map[string]string {
	out := map[string]string{}
	var raw []byte
	err := c.db.QueryRow(ctx,
		`SELECT column_map FROM csv_column_mappings WHERE client_book_id = $1 ORDER BY created_at DESC LIMIT 1`,
		bookID).Scan(&raw)
	if err != nil {
		return out // no mapping yet — CSV rows parse structurally but amount defaults
	}
	_ = json.Unmarshal(raw, &out)
	return out
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
				 counterparty, description, gl_account_code, page_number, bbox, extraction_confidence, source_format)
			 VALUES ($1, $2, $3, $4, $5, NULLIF($6,''), NULLIF($7,''), NULLIF($8,''),
			 	$9, '{}', $10, $11)`,
			bookID, docID, e.EntityType, e.AmountCents, txnDate,
			e.Counterparty, e.Description, e.GlAccountCode,
			e.PageNumber, e.Confidence, e.SourceFormat)
		if err != nil {
			return err
		}
	}
	slog.Info("coordinator: persisted entities", "doc", docID, "count", len(ents))
	return nil
}