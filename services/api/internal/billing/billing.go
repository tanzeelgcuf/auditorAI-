package billing

// Stripe billing — subscription per firm (docs 06 §9 tiers). v1 keeps billing
// simple: flat per-firm subscription via Stripe Checkout, webhook syncs status.

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	stripe "github.com/stripe/stripe-go/v79"
	"github.com/stripe/stripe-go/v79/checkout/session"
	"github.com/stripe/stripe-go/v79/webhook"
	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/middleware"
)

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

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// PriceIDs per tier (replace with your Stripe price IDs in prod env).
const (
	priceStarter = "price_starter_5books"
	priceGrowth  = "price_growth_20books"
	priceScale   = "price_scale_unlimited"
)

// HandleCheckout creates a Stripe Checkout session for a firm subscription.
func (s *Service) HandleCheckout(w http.ResponseWriter, r *http.Request) {
	firmID := middleware.GetFirmID(r.Context())
	if firmID == "" {
		writeProblem(w, http.StatusUnauthorized, "https://ai-auditor.dev/errors/unauthorized", "unauthorized")
		return
	}

	secret := os.Getenv("STRIPE_SECRET_KEY")
	if secret == "" {
		writeProblem(w, http.StatusServiceUnavailable, "https://ai-auditor.dev/errors/not-configured",
			"billing not configured")
		return
	}
	stripe.Key = secret

	var req struct {
		Tier string `json:"tier"` // starter | growth | scale
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "https://ai-auditor.dev/errors/bad-request", "invalid body")
		return
	}
	priceID := priceStarter
	switch req.Tier {
	case "starter":
		priceID = priceStarter
	case "growth":
		priceID = priceGrowth
	case "scale":
		priceID = priceScale
	default:
		writeProblem(w, http.StatusBadRequest, "https://ai-auditor.dev/errors/bad-request",
			"tier must be starter, growth, or scale")
		return
	}

	params := &stripe.CheckoutSessionParams{
		Mode: stripe.String(string(stripe.CheckoutSessionModeSubscription)),
		LineItems: []*stripe.CheckoutSessionLineItemParams{
			{Price: stripe.String(priceID), Quantity: stripe.Int64(1)},
		},
		SuccessURL: stripe.String(os.Getenv("BILLING_SUCCESS_URL")),
		CancelURL:  stripe.String(os.Getenv("BILLING_CANCEL_URL")),
		Metadata: map[string]string{
			"firm_id": firmID,
		},
	}
	cs, err := session.New(params)
	if err != nil {
		slog.Error("stripe checkout session failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "checkout failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": cs.URL})
}

// HandleStripeWebhook receives Stripe events (checkout.session.completed,
// customer.subscription.updated) and syncs firm subscription state.
func (s *Service) HandleStripeWebhook(w http.ResponseWriter, r *http.Request) {
	secret := os.Getenv("STRIPE_WEBHOOK_SECRET")
	if secret == "" {
		writeProblem(w, http.StatusServiceUnavailable, "https://ai-auditor.dev/errors/not-configured",
			"webhook not configured")
		return
	}

	payload, err := readAllBody(r)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "https://ai-auditor.dev/errors/bad-request", "read failed")
		return
	}
	event, err := webhook.ConstructEvent(payload, r.Header.Get("Stripe-Signature"), secret)
	if err != nil {
		slog.Warn("stripe webhook signature invalid", "error", err)
		writeProblem(w, http.StatusBadRequest, "https://ai-auditor.dev/errors/bad-request", "invalid signature")
		return
	}

	switch event.Type {
	case "checkout.session.completed":
		var cs stripe.CheckoutSession
		if err := json.Unmarshal(event.Data.Raw, &cs); err != nil {
			slog.Error("webhook parse failed", "error", err)
			break
		}
		firmID := cs.Metadata["firm_id"]
		if firmID == "" {
			slog.Warn("checkout completed without firm_id metadata")
			break
		}
		_, err := s.db.Exec(r.Context(),
			`UPDATE firms SET stripe_customer_id = $1 WHERE id = $2`,
			cs.Customer, firmID)
		if err != nil {
			slog.Error("failed to sync stripe customer", "error", err)
		}
	case "customer.subscription.updated", "customer.subscription.deleted":
		var sub stripe.Subscription
		if err := json.Unmarshal(event.Data.Raw, &sub); err != nil {
			slog.Error("webhook parse failed", "error", err)
			break
		}
		slog.Info("subscription updated", "customer", sub.Customer, "status", sub.Status)
	default:
		slog.Debug("ignored stripe event", "type", event.Type)
	}

	writeJSON(w, http.StatusOK, map[string]string{"received": "true"})
}

func readAllBody(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	buf := make([]byte, 0, 65536)
	tmp := make([]byte, 32768)
	for {
		n, err := r.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			if err.Error() == "EOF" {
				return buf, nil
			}
			return nil, err
		}
	}
}
