package push

// Mobile push bridge (doc 03 §3.10 / doc 07 §8). High-severity findings page a
// controller via Expo push. Device tokens are registered by the mobile app and
// stored per-firm; SendFindingAlert fans out to Expo's push service.

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/middleware"
)

const expoPushURL = "https://exp.host/--/api/v2/push/send"

type Service struct {
	db *pgxpool.Pool
}

func NewService() *Service { return &Service{} }

func (s *Service) SetDB(db *pgxpool.Pool) { s.db = db }

func writeProblem(w http.ResponseWriter, status int, typ, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"type": typ, "title": http.StatusText(status), "status": status, "detail": detail,
	})
}

// HandleRegisterDevice upserts a mobile device token for the authenticated user's firm.
func (s *Service) HandleRegisterDevice(w http.ResponseWriter, r *http.Request) {
	userID := middleware.GetUserID(r.Context())
	firmID := middleware.GetFirmID(r.Context())
	if userID == "" || firmID == "" {
		writeProblem(w, http.StatusUnauthorized, "https://ai-auditor.dev/errors/unauthorized", "unauthorized")
		return
	}

	var req struct {
		DeviceToken string `json:"device_token"`
		Platform    string `json:"platform"` // ios | android
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "https://ai-auditor.dev/errors/bad-request", "invalid body")
		return
	}
	if !validToken(req.DeviceToken) {
		writeProblem(w, http.StatusBadRequest, "https://ai-auditor.dev/errors/bad-request", "invalid device_token")
		return
	}
	if req.Platform != "ios" && req.Platform != "android" {
		writeProblem(w, http.StatusBadRequest, "https://ai-auditor.dev/errors/bad-request", "platform must be ios or android")
		return
	}

	c := middleware.GetConn(r.Context())
	db := c
	if db == nil {
		acquired, err := s.db.Acquire(r.Context())
		if err != nil {
			writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "no db conn")
			return
		}
		defer acquired.Release()
		db = acquired
	}

	_, err := db.Exec(r.Context(),
		`INSERT INTO device_tokens (firm_id, user_id, token, platform)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (token) DO UPDATE SET last_seen_at = now()`,
		firmID, userID, req.DeviceToken, req.Platform)
	if err != nil {
		slog.Error("failed to register device token", "error", err)
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "insert failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// SendFindingAlert pages high-severity findings to the firm's registered devices.
func (s *Service) SendFindingAlert(ctx context.Context, firmID, findingID, severity, bookID, summary string) error {
	if !severityShouldNotify(severity) {
		return nil // only high severity pages a human
	}
	secret := os.Getenv("EXPONENT_ACCESS_TOKEN")
	if secret == "" {
		slog.Warn("EXPONENT_ACCESS_TOKEN unset — push skipped")
		return nil
	}

	rows, err := s.db.Query(ctx,
		`SELECT token FROM device_tokens WHERE firm_id = $1 AND last_seen_at > now() - interval '90 days'`,
		firmID)
	if err != nil {
		return err
	}
	defer rows.Close()

	var tokens []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return err
		}
		tokens = append(tokens, t)
	}
	if len(tokens) == 0 {
		return nil
	}

	body := BuildExpoPayload(tokens, findingID, bookID, severity, summary)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, expoPushURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+secret)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		slog.Warn("expo push rejected", "status", resp.StatusCode)
		return nil
	}
	slog.Info("push sent", "finding_id", findingID, "devices", len(tokens))
	return nil
}

// severityShouldNotify gates alerts — only high-severity pages a human (doc 03 §3.10).
func severityShouldNotify(severity string) bool {
	return severity == "high"
}

// validToken enforces the Expo push token shape: non-empty, 22-150 chars, no whitespace.
func validToken(t string) bool {
	if len(t) < 22 || len(t) > 150 {
		return false
	}
	return !strings.ContainsAny(t, " \t\r\n")
}

// BuildExpoPayload builds the Expo push send body — one message per device token.
func BuildExpoPayload(tokens []string, findingID, bookID, severity, summary string) []byte {
	type message struct {
		To    string            `json:"to"`
		Title string            `json:"title"`
		Body  string            `json:"body"`
		Data  map[string]string `json:"data"`
	}
	_ = severity
	messages := make([]message, 0, len(tokens))
	for _, t := range tokens {
		messages = append(messages, message{
			To:    t,
			Title: "High-severity finding",
			Body:  summary,
			Data:  map[string]string{"finding_id": findingID, "book_id": bookID},
		})
	}
	b, err := json.Marshal(messages)
	if err != nil {
		return []byte("[]")
	}
	return b
}
