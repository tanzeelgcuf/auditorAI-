package pipeline

// Orchestrates the document pipeline via NATS JetStream:
//   upload -> ingestion.completed -> entity.extraction.requested -> findings
// The API service coordinates; agent-runtime consumes the extraction event.

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

type EventClient struct {
	nc *nats.Conn
	js jetstream.JetStream
}

func NewEventClient(natsURL string) (*EventClient, error) {
	nc, err := nats.Connect(natsURL)
	if err != nil {
		return nil, err
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, err
	}
	// Ensure the pipeline streams exist (idempotent) so publishes never fail
	// with "no response from stream" on a fresh NATS instance. DOCUMENTS is a
	// WorkQueue (single-consumer: the coordinator). Verification lives on its
	// own stream because a WorkQueue allows only one consumer, and the verify
	// worker is a second consumer.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      "DOCUMENTS",
		Subjects:  []string{"document.uploaded", "document.processing.failed", "ingestion.completed", "entity.extraction.requested"},
		Retention: jetstream.WorkQueuePolicy,
	})
	_, _ = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      "VERIFY",
		Subjects:  []string{"verification.requested"},
		Retention: jetstream.WorkQueuePolicy,
	})
	return &EventClient{nc: nc, js: js}, nil
}

func (e *EventClient) Close() {
	if e.nc != nil {
		e.nc.Close()
	}
}

// Publish sends a raw event on a subject (used by documents upload).
func (e *EventClient) Publish(ctx context.Context, subject string, payload []byte) (*jetstream.PubAck, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return e.js.Publish(ctx, subject, payload)
}

// RequestExtraction publishes an event that agent-runtime consumes to run the
// extract -> classify -> link -> verify graph for a batch.
func (e *EventClient) RequestExtraction(ctx context.Context, clientBookID, batchID string) error {
	payload, err := json.Marshal(map[string]string{
		"client_book_id": clientBookID,
		"batch_id":       batchID,
	})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err = e.js.Publish(ctx, "entity.extraction.requested", payload)
	if err != nil {
		slog.Error("failed to publish extraction request", "error", err)
		return err
	}
	slog.Info("extraction requested", "client_book_id", clientBookID, "batch_id", batchID)
	return nil
}

// RequestVerification publishes a verify request for a reconciliation group.
func (e *EventClient) RequestVerification(ctx context.Context, groupID, clientBookID string) error {
	payload, err := json.Marshal(map[string]string{
		"group_id":        groupID,
		"client_book_id": clientBookID,
	})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err = e.js.Publish(ctx, "verification.requested", payload)
	return err
}

// PublishVerification satisfies mcp.VerificationPublisher.
func (e *EventClient) PublishVerification(ctx context.Context, groupID, clientBookID string) error {
	return e.RequestVerification(ctx, groupID, clientBookID)
}
