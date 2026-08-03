// Package email sends transactional mail through Resend (doc 12 §4).
// Provider sits behind EmailSender so it can be swapped without touching call
// sites. Uses Resend's REST API directly (one endpoint) — no SDK dependency.
package email

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

const resendBase = "https://api.resend.com/emails"

// EmailSender is the provider-agnostic mail interface. Implementations (Resend,
// Postmark, SES) are swappable at construction without touching call sites.
type EmailSender interface {
	Send(ctx context.Context, to, subject, html string) error
}

// Config holds the Resend credentials. Supplying zero values yields a no-op
// sender that only logs (safe for local/dev where RESEND_API_KEY is unset).
type Config struct {
	APIKey   string
	From     string // "AI Auditor <noreply@auditor.app>"
	FromName string
}

// Resend is the EmailSender backed by Resend's REST API.
type Resend struct {
	apiKey string
	from   string
	client *http.Client
}

// NewResend builds the sender from env (RESEND_API_KEY, EMAIL_FROM,
// EMAIL_FROM_NAME). When the key is absent it returns a *DevSender* that logs
// instead of sending — signup/reset still complete locally, nothing breaks.
func NewResend() EmailSender {
	key := os.Getenv("RESEND_API_KEY")
	if key == "" {
		slog.Warn("RESEND_API_KEY not set — emails logged, not sent")
		return Noop{}
	}
	from := os.Getenv("EMAIL_FROM")
	if from == "" {
		from = "AI Auditor <noreply@auditor.app>"
	}
	name := os.Getenv("EMAIL_FROM_NAME")
	if name != "" {
		from = name + " <" + strings.Trim(from, "<>") + ">"
	}
	return &Resend{
		apiKey: key,
		from:   from,
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

func (s *Resend) Send(ctx context.Context, to, subject, html string) error {
	body, _ := json.Marshal(map[string]any{
		"from":    s.from,
		"to":      []string{to},
		"subject": subject,
		"html":    html,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, resendBase, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("resend: %w", err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("resend: HTTP %d: %v", resp.StatusCode, out)
	}
	slog.Info("email sent", "to", to, "subject", subject)
	return nil
}

// Noop logs sends but does not hit the network — the local/dev mail path.
type Noop struct{}

func (Noop) Send(_ context.Context, to, subject, html string) error {
	slog.Info("email(noop)", "to", to, "subject", subject, "body", truncate(html, 120))
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}