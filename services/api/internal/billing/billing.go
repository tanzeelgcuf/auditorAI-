package billing

// Stripe billing — subscription per firm (docs 06 §9 tiers). v1 keeps billing
// simple: flat per-firm subscription via Stripe Checkout, webhook syncs status.

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	stripe "github.com/stripe/stripe-go/v79"
	"github.com/stripe/stripe-go/v79/checkout/session"
	"github.com/stripe/stripe-go/v79/webhook"
	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/middleware"
)

type Service struct {
	db *pgxpool.Pool
	// sysDB is the BYPASSRLS pool, used ONLY by HandleStripeWebhook. A Stripe
	// delivery carries no JWT, so no RLSInjector connection exists on its request
	// and app.current_firm is unset — and the `firms` policy
	// (infra/init.sql:466) calls current_setting('app.current_firm') WITHOUT the
	// missing_ok argument, so on an unset GUC it RAISES instead of returning NULL.
	// Writing through the RLS-bound pool therefore errors rather than silently
	// matching zero rows. Every other handler in this file uses `db`.
	sysDB *pgxpool.Pool
}

func NewService() *Service { return &Service{} }

func (s *Service) SetDB(db *pgxpool.Pool) { s.db = db }

// SetSysDB provides the BYPASSRLS pool for webhook-driven writes. Wired in
// cmd/server/main.go alongside SetDB.
func (s *Service) SetSysDB(db *pgxpool.Pool) { s.sysDB = db }

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

// tierForPrice maps a Stripe price ID back to the tier name persisted on the
// firm. Returns "" for an unrecognised price, and callers store NULL rather than
// guessing — an unknown price means the Stripe catalogue and these constants have
// drifted, and inventing a tier there would silently grant or deny entitlements.
func tierForPrice(priceID string) string {
	switch priceID {
	case priceStarter:
		return "starter"
	case priceGrowth:
		return "growth"
	case priceScale:
		return "scale"
	default:
		return ""
	}
}

// stripeEnvelope is the subset of the webhook payload this handler reads that is
// not already read through the SDK structs elsewhere in this file.
//
// It is parsed from the raw (signature-verified) bytes rather than from
// stripe.Subscription fields on purpose. The wire format — `id`, `created`, and
// the object's `status`, `customer`, `current_period_end`, `items.data[].price.id`
// — is versioned, documented REST surface. Go struct paths are not: stripe-go has
// moved fields between majors (current_period_end moved from the subscription to
// the subscription item in a later release), and this repo has no vendor
// directory or module cache, so a struct path cannot be checked at review time
// the way a documented JSON key can.
type stripeEnvelope struct {
	ID      string `json:"id"`
	Created int64  `json:"created"`
	Data    struct {
		Object struct {
			Customer          string            `json:"customer"`
			Status            string            `json:"status"`
			Metadata          map[string]string `json:"metadata"`
			CancelAtPeriodEnd bool              `json:"cancel_at_period_end"`
			CurrentPeriodEnd  int64             `json:"current_period_end"`
			Items             struct {
				Data []struct {
					Price struct {
						ID string `json:"id"`
					} `json:"price"`
				} `json:"data"`
			} `json:"items"`
		} `json:"object"`
	} `json:"data"`
}

// priceID returns the first line item's price, or "" when the payload carries no
// items (subscription.deleted payloads sometimes do not).
func (e *stripeEnvelope) priceID() string {
	if len(e.Data.Object.Items.Data) == 0 {
		return ""
	}
	return e.Data.Object.Items.Data[0].Price.ID
}

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

	// Reuse the firm's existing Stripe customer if it already has one.
	//
	// Without this, a second call to /v1/billing/checkout makes Stripe create a
	// SECOND customer for the same firm. checkout.session.completed then overwrites
	// firms.stripe_customer_id with the new one, and every subsequent subscription
	// event for the first customer arrives for a customer no firm carries — which
	// the webhook now (correctly) answers 500 to, so Stripe retries it forever.
	// The firm ends up with two live subscriptions and one of them unreadable.
	//
	// Read through the RLS-bound pool on purpose: this request has a JWT and an
	// RLSInjector connection, and the `firms` policy restricts it to its own row.
	// This is the only reader of s.db in this file.
	var existingCustomer *string
	if err := middleware.DB(r.Context(), s.db).QueryRow(r.Context(),
		`SELECT stripe_customer_id FROM firms WHERE id = $1`, firmID,
	).Scan(&existingCustomer); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		slog.Error("failed to read existing stripe customer", "firm_id", firmID, "error", err)
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal",
			"checkout failed")
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
	if existingCustomer != nil && *existingCustomer != "" {
		params.Customer = stripe.String(*existingCustomer)
	}
	cs, err := session.New(params)
	if err != nil {
		slog.Error("stripe checkout session failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "checkout failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": cs.URL})
}

// HandleStripeWebhook receives Stripe events and syncs firm subscription state.
//
// Registered as a PUBLIC route (cmd/server/main.go) because Stripe cannot present
// a JWT. It authenticates its caller the way Stripe intends and refuses to fail
// open: no STRIPE_WEBHOOK_SECRET -> 503 with no processing, and
// webhook.ConstructEvent verifies the Stripe-Signature HMAC over the raw body.
//
// Status codes are load-bearing here. Stripe treats any 2xx as "delivered" and
// stops retrying. This handler used to answer 200 unconditionally, including
// after a swallowed database error — so a failed write meant billing state
// diverged from Stripe permanently, with a single slog.Error line as the only
// trace. Every path that fails to persist now returns 500 so Stripe retries.
func (s *Service) HandleStripeWebhook(w http.ResponseWriter, r *http.Request) {
	secret := os.Getenv("STRIPE_WEBHOOK_SECRET")
	if secret == "" {
		writeProblem(w, http.StatusServiceUnavailable, "https://ai-auditor.dev/errors/not-configured",
			"webhook not configured")
		return
	}
	if s.sysDB == nil {
		// Fail closed rather than write through the RLS-bound pool: see the sysDB
		// field comment. A 500 makes Stripe retry, so a misconfigured deployment
		// loses no events once it is corrected.
		slog.Error("stripe webhook has no sysDB pool; refusing to process event")
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/not-configured",
			"billing store not configured")
		return
	}

	payload, err := readWebhookBody(w, r)
	if err != nil {
		slog.Warn("stripe webhook body read failed", "error", err)
		writeProblem(w, http.StatusBadRequest, "https://ai-auditor.dev/errors/bad-request", "read failed")
		return
	}
	event, err := webhook.ConstructEvent(payload, r.Header.Get("Stripe-Signature"), secret)
	if err != nil {
		slog.Warn("stripe webhook signature invalid", "error", err)
		writeProblem(w, http.StatusBadRequest, "https://ai-auditor.dev/errors/bad-request", "invalid signature")
		return
	}

	// Parsed only after ConstructEvent succeeds, so these bytes are HMAC-verified.
	var env stripeEnvelope
	if err := json.Unmarshal(payload, &env); err != nil {
		slog.Warn("stripe webhook envelope parse failed", "type", event.Type, "error", err)
	}

	switch event.Type {
	case "checkout.session.completed":
		// Read from the envelope, not stripe.CheckoutSession. `customer` is an
		// EXPANDABLE field: on the wire it is a string id (webhooks are never sent
		// expanded), but stripe-go models expandable fields as a pointer to the
		// object. The code this replaced passed `cs.Customer` straight into
		// `UPDATE firms SET stripe_customer_id = $1` — and because pgx's Exec takes
		// ...any, that compiles and fails at runtime on encode. If that is what the
		// type is, this write could never have succeeded, which is a second and
		// independent reason billing state never synced. Reading the documented
		// string key removes the question entirely.
		firmID := env.Data.Object.Metadata["firm_id"]
		if firmID == "" {
			// Not retryable: the metadata Stripe echoed back is what HandleCheckout
			// set, so a missing firm_id will be missing on every redelivery. 200
			// stops the retry loop; the log line is the signal that HandleCheckout
			// and this handler have drifted.
			slog.Error("checkout completed without firm_id metadata", "event", env.ID)
			writeJSON(w, http.StatusOK, map[string]string{"received": "true", "handled": "false"})
			return
		}
		if !s.applyCheckout(r, w, firmID, &env) {
			return
		}
	case "customer.subscription.created", "customer.subscription.updated",
		"customer.subscription.deleted":
		// These used to be a bare slog.Info. Nothing was persisted, so the
		// application could not answer "is this firm's subscription active?" at
		// all — cancellations and payment failures were logged and discarded.
		//
		// .created is included because it is the event that carries the initial
		// price and status; without it a new firm's tier stayed NULL until some
		// unrelated later update happened to arrive.
		if !s.applySubscription(r, w, &env) {
			return
		}
	default:
		slog.Debug("ignored stripe event", "type", event.Type)
	}

	writeJSON(w, http.StatusOK, map[string]string{"received": "true"})
}

// applyCheckout records the Stripe customer on the firm. Returns false when it
// has already written a response.
//
// Deliberately writes ONLY stripe_customer_id and does not touch
// subscription_event_at. That watermark guards the subscription state columns,
// and checkout.session.completed describes a different object with its own
// unrelated `created` timestamp — sharing one watermark between them could make a
// late-arriving checkout event silence a legitimate later subscription update.
func (s *Service) applyCheckout(r *http.Request, w http.ResponseWriter, firmID string, env *stripeEnvelope) bool {
	customer := env.Data.Object.Customer
	if customer == "" {
		// Not retryable: a completed Checkout session in subscription mode always
		// carries a customer, so an empty one means the payload shape changed.
		slog.Error("checkout completed without a customer id", "event", env.ID, "firm_id", firmID)
		writeJSON(w, http.StatusOK, map[string]string{"received": "true", "handled": "false"})
		return false
	}
	tag, err := s.sysDB.Exec(r.Context(),
		`UPDATE firms SET stripe_customer_id = $1 WHERE id = $2`,
		customer, firmID)
	if err != nil {
		slog.Error("failed to sync stripe customer", "firm_id", firmID, "error", err)
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal",
			"failed to persist customer")
		return false
	}
	if tag.RowsAffected() == 0 {
		// Not retryable: sysDB is BYPASSRLS, so zero rows means no firm has this id,
		// and redelivery cannot change that.
		slog.Error("checkout completed for unknown firm", "firm_id", firmID)
		writeJSON(w, http.StatusOK, map[string]string{"received": "true", "handled": "false"})
		return false
	}
	slog.Info("stripe customer linked to firm", "firm_id", firmID)
	return true
}

// applySubscription persists subscription state onto the firm that owns the
// Stripe customer. Returns false when it has already written a response.
//
// Keyed on stripe_customer_id because subscription events carry a customer, not
// the firm_id metadata that only checkout.session.completed echoes back.
func (s *Service) applySubscription(r *http.Request, w http.ResponseWriter, env *stripeEnvelope) bool {
	customer := env.Data.Object.Customer
	if customer == "" {
		slog.Error("stripe subscription event carried no customer id", "event", env.ID)
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal",
			"event missing customer")
		return false
	}
	eventAt := time.Unix(env.Created, 0).UTC()

	var firmID string
	var lastEventAt *time.Time
	err := s.sysDB.QueryRow(r.Context(),
		`SELECT id, subscription_event_at FROM firms WHERE stripe_customer_id = $1`,
		customer).Scan(&firmID, &lastEventAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// No firm carries this customer yet. The usual cause is ordering: Stripe
		// does not guarantee it, so customer.subscription.created can beat
		// checkout.session.completed, which is what writes the link. 500 makes
		// Stripe retry, and the retry succeeds once the link lands. Answering 200
		// here would drop the firm's subscription state permanently.
		slog.Error("stripe subscription event for a customer not linked to any firm",
			"event", env.ID, "customer", customer)
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal",
			"customer not linked to a firm")
		return false
	case err != nil:
		slog.Error("failed to look up firm for stripe customer", "event", env.ID, "error", err)
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal",
			"lookup failed")
		return false
	}

	// Out-of-order guard. Stripe explicitly does not guarantee event ordering, so a
	// stale "active" can arrive after a "canceled". Without this watermark the last
	// event to be *delivered* wins rather than the last event to have *happened*.
	if lastEventAt != nil && lastEventAt.After(eventAt) {
		slog.Info("ignored out-of-order stripe subscription event",
			"event", env.ID, "firm_id", firmID,
			"event_at", eventAt, "already_applied", *lastEventAt)
		return true
	}

	return s.writeSubscriptionState(r, w, firmID, env, eventAt)
}

// writeSubscriptionState performs the UPDATE. Split out so applySubscription
// stays readable; it has no other caller.
func (s *Service) writeSubscriptionState(r *http.Request, w http.ResponseWriter,
	firmID string, env *stripeEnvelope, eventAt time.Time) bool {

	var tier *string
	if t := tierForPrice(env.priceID()); t != "" {
		tier = &t
	}
	var periodEnd *time.Time
	if env.Data.Object.CurrentPeriodEnd > 0 {
		pe := time.Unix(env.Data.Object.CurrentPeriodEnd, 0).UTC()
		periodEnd = &pe
	}
	var status *string
	if st := env.Data.Object.Status; st != "" {
		status = &st
	}

	// COALESCE on tier keeps a previously known tier when an event arrives with no
	// line items — subscription.deleted payloads sometimes omit them, and an
	// unrecognised price ID maps to NULL rather than a guess (see tierForPrice).
	tag, err := s.sysDB.Exec(r.Context(), `
		UPDATE firms
		   SET subscription_status            = $1,
		       subscription_tier              = COALESCE($2, subscription_tier),
		       subscription_current_period_end = $3,
		       subscription_cancel_at_period_end = $4,
		       subscription_event_at          = $5
		 WHERE id = $6`,
		status, tier, periodEnd, env.Data.Object.CancelAtPeriodEnd, eventAt, firmID)
	if err != nil {
		slog.Error("failed to persist subscription state",
			"event", env.ID, "firm_id", firmID, "error", err)
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal",
			"failed to persist subscription state")
		return false
	}
	if tag.RowsAffected() == 0 {
		// The firm was found by the SELECT immediately above, so zero rows here means
		// it disappeared between the two statements. Retryable.
		slog.Error("subscription update matched no firm", "event", env.ID, "firm_id", firmID)
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal",
			"firm vanished mid-update")
		return false
	}
	slog.Info("subscription state persisted", "event", env.ID, "firm_id", firmID,
		"status", env.Data.Object.Status, "cancel_at_period_end", env.Data.Object.CancelAtPeriodEnd)
	return true
}

//
// Two reasons this is not the old unbounded loop. First, this endpoint is now
// public (cmd/server/main.go) — anything on the internet can POST to it, and the
// body is read into memory BEFORE the signature is checked, because HMAC
// verification needs the raw bytes. An unbounded read there is a
// memory-exhaustion vector that no amount of signature verification prevents.
// Second, the loop it replaces compared `err.Error() == "EOF"` — a string
// comparison against an error message — instead of errors.Is(err, io.EOF), so
// any wrapped EOF would have been returned as a hard failure.
//
// Stripe does not publish a maximum event size; real payloads are single-digit
// KB. 1 MiB is ~100x headroom and still finite.
const maxWebhookBody = 1 << 20

// readWebhookBody reads at most maxWebhookBody bytes. MaxBytesReader (not a
// plain io.LimitReader) is deliberate: it makes the reader return an error at
// the ceiling instead of silently truncating, so an oversized body is rejected
// as a bad request rather than failing signature verification for a reason the
// logs would misattribute.
func readWebhookBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	return io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
}
