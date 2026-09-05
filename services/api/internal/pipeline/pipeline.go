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
	// with "no response from stream" on a fresh NATS instance.
	//
	// RETENTION IS PER-STREAM AND IT MATTERS. A WorkQueue stream deletes a
	// message only when a consumer acks it, so a subject on a WorkQueue stream
	// with NO consumer accumulates forever. DOCUMENTS previously carried three
	// subjects under WorkQueuePolicy, and two of them — ingestion.completed
	// (published by services/ingestion/src/grpc/mod.rs:204) and
	// document.processing.failed — have no consumer anywhere in the repository.
	// Those messages were unbounded, unread growth in the NATS store.
	//
	// So DOCUMENTS now carries only the subject its single consumer actually
	// reads, and the two notification subjects move to a LIMITS stream where
	// unconsumed messages age out instead of piling up.
	//
	// MIGRATION NOTE, deliberately explicit: CreateStream does NOT rewrite the
	// config of a stream that already exists. On an existing NATS deployment
	// DOCUMENTS keeps its three subjects, PIPELINE_EVENTS then fails to create
	// with "subjects overlap", and the old behaviour persists. Fresh deployments
	// get this right; an existing one needs the DOCUMENTS stream updated (or
	// deleted while drained) once. The error is now logged rather than discarded,
	// which is what makes that state visible instead of silent.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	mkStream := func(cfg jetstream.StreamConfig) {
		if _, err := js.CreateStream(ctx, cfg); err != nil {
			// Not fatal: the common case is "already exists with this config".
			// Logged because the OTHER cases — an existing stream with a different
			// subject list, or an overlap rejection — used to be invisible.
			slog.Warn("jetstream stream not created (may already exist with a "+
				"different config — see the migration note in pipeline.go)",
				"stream", cfg.Name, "subjects", cfg.Subjects, "error", err)
		}
	}
	// DOCUMENTS is a WorkQueue with exactly one consumer: the coordinator.
	mkStream(jetstream.StreamConfig{
		Name:      "DOCUMENTS",
		Subjects:  []string{"document.uploaded"},
		Retention: jetstream.WorkQueuePolicy,
	})
	// Notification subjects: produced, not consumed. Limits retention with an
	// explicit age and count so they are bounded without a consumer. They are
	// still worth publishing — an operator tailing document.processing.failed is
	// how a stuck pilot book gets noticed — but they must not be a WorkQueue.
	mkStream(jetstream.StreamConfig{
		Name:      "PIPELINE_EVENTS",
		Subjects:  []string{"document.processing.failed", "ingestion.completed"},
		Retention: jetstream.LimitsPolicy,
		MaxAge:    7 * 24 * time.Hour,
		MaxMsgs:   100_000,
	})
	mkStream(jetstream.StreamConfig{
		Name:      "EXTRACTION",
		Subjects:  []string{"entity.extraction.requested"},
		Retention: jetstream.WorkQueuePolicy,
	})
	// Book-wide linking lives on its own stream (a WorkQueue allows one
	// consumer; per-doc extraction and book-wide link need separate ones).
	mkStream(jetstream.StreamConfig{
		Name:      "LINK",
		Subjects:  []string{"link.requested"},
		Retention: jetstream.WorkQueuePolicy,
	})
	mkStream(jetstream.StreamConfig{
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
