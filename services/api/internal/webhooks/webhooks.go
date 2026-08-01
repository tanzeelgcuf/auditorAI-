package webhooks

// Webhook delivery engine (doc 07 §7) — delivers finding.created and
// report.generated events to firm-subscribed endpoints with HMAC-SHA256
// signatures, retry with backoff, and auto-disable after repeated failures.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Service struct {
	db *pgxpool.Pool
}

func NewService() *Service { return &Service{} }

func (s *Service) SetDB(db *pgxpool.Pool) { s.db = db }

// Subscription mirrors a row of webhook_subscriptions (fields needed for delivery).
type Subscription struct {
	ID                 string
	FirmID             string
	TargetURL          string
	EventTypes         []string
	SigningSecret      string
	ConsecutiveFailures int
	Enabled            bool
}

// NotifyFindingCreated delivers the finding.created event to matching subscriptions.
func (s *Service) NotifyFindingCreated(ctx context.Context, firmID, findingID, severity string) error {
	payload := map[string]any{
		"finding_id": findingID,
		"severity":   severity,
	}
	return s.deliverEvent(ctx, firmID, "finding.created", payload)
}

// NotifyReportGenerated delivers the report.generated event to matching subscriptions.
func (s *Service) NotifyReportGenerated(ctx context.Context, firmID, reportID string) error {
	payload := map[string]any{"report_id": reportID}
	return s.deliverEvent(ctx, firmID, "report.generated", payload)
}

func (s *Service) deliverEvent(ctx context.Context, firmID, eventType string, payload map[string]any) error {
	subs, err := s.matchingSubscriptions(ctx, firmID, eventType)
	if err != nil {
		return err
	}
	for i := range subs {
		if err := s.deliver(ctx, &subs[i], eventType, payload); err != nil {
			slog.Warn("webhook delivery failed", "sub", subs[i].ID, "event", eventType, "error", err)
			if err := s.recordFailure(ctx, subs[i].ID); err != nil {
				slog.Warn("failed to record webhook failure", "sub", subs[i].ID, "error", err)
			}
		}
	}
	return nil
}

func (s *Service) matchingSubscriptions(ctx context.Context, firmID, eventType string) ([]Subscription, error) {
	rows, err := s.db.Query(ctx,
		`SELECT id::text, firm_id::text, target_url, event_types, signing_secret,
			consecutive_failures, enabled
		 FROM webhook_subscriptions
		 WHERE firm_id = $1 AND enabled = true AND $2 = ANY(event_types)`, firmID, eventType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var subs []Subscription
	for rows.Next() {
		var sub Subscription
		if err := rows.Scan(&sub.ID, &sub.FirmID, &sub.TargetURL, &sub.EventTypes,
			&sub.SigningSecret, &sub.ConsecutiveFailures, &sub.Enabled); err != nil {
			return nil, err
		}
		subs = append(subs, sub)
	}
	return subs, rows.Err()
}

// deliver posts a signed event to one subscription, retrying with backoff on
// 5xx/network errors. Returns nil on success, error after retries exhausted.
func (s *Service) deliver(ctx context.Context, sub *Subscription, eventType string, payload map[string]any) error {
	body, err := json.Marshal(map[string]any{"event": eventType, "data": payload})
	if err != nil {
		return err
	}

	backoffs := []time.Duration{300 * time.Millisecond, 900 * time.Millisecond, 2700 * time.Millisecond}
	var lastErr error

	for attempt := 0; attempt <= len(backoffs); attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.TargetURL, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-AI-Auditor-Signature", sign(sub.SigningSecret, body))

		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
		} else {
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return nil
			}
			if resp.StatusCode < 500 {
				// 4xx = bad subscription config, no retry
				return fmt.Errorf("target rejected with %d", resp.StatusCode)
			}
			lastErr = fmt.Errorf("target returned %d", resp.StatusCode)
		}

		if attempt < len(backoffs) {
			select {
			case <-time.After(backoffs[attempt]):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return fmt.Errorf("delivery failed after retries: %w", lastErr)
}

func (s *Service) recordFailure(ctx context.Context, subID string) error {
	var failures int
	err := s.db.QueryRow(ctx,
		`UPDATE webhook_subscriptions SET consecutive_failures = consecutive_failures + 1
		 WHERE id = $1 RETURNING consecutive_failures`, subID).Scan(&failures)
	if err != nil {
		return err
	}
	if failures >= 3 {
		_, err = s.db.Exec(ctx,
			`UPDATE webhook_subscriptions SET enabled = false WHERE id = $1`, subID)
		if err != nil {
			return err
		}
		slog.Warn("webhook auto-disabled after repeated failures", "sub", subID)
	}
	return nil
}

// Sign computes the HMAC-SHA256 hex signature for a body with the given secret.
func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// ensure pgx import used (matchingSubscriptions error path references it)
var _ = pgx.ErrNoRows
