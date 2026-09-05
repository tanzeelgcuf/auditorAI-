package billing

// Tests for the Stripe webhook. There were none before 2026-09-04, which is part
// of why the handler could answer 200 to a failed database write for as long as it
// did.
//
// WHAT THESE COVER, AND WHAT THEY DELIBERATELY DO NOT
//
// Covered: every pre-persistence rejection path (missing secret, missing sysDB,
// bad signature, oversized body), the envelope parse against a realistic Stripe
// payload, the price->tier mapping, and — against a real Postgres — the two
// behaviours that were bugs: an unlinked customer must NOT be answered 200, and a
// stale event must not overwrite newer state.
//
// NOT covered: the full path through webhook.ConstructEvent with a valid
// signature. stripe-go compares the payload's `api_version` against the version
// the SDK pins, and this environment has no module cache to read that constant
// from, so a test that signed a payload would be asserting a version string
// guessed at review time. The DB-backed tests below therefore call
// applyCheckout/applySubscription directly with a parsed envelope, which is the
// same code the switch in HandleStripeWebhook reaches.
//
// That api_version comparison is itself worth checking on first deploy: if the
// Stripe dashboard's endpoint version differs from stripe-go v79's pinned version,
// ConstructEvent fails and every event 400s.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// dsnFreePool returns a non-nil *pgxpool.Pool that never opens a connection.
// pgxpool.New only parses the DSN and creates the pool lazily, so this is enough
// to get past the `s.sysDB == nil` guard in tests that must not reach a database.
func dsnFreePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(),
		"postgres://unused:unused@127.0.0.1:1/unused?connect_timeout=1")
	if err != nil {
		t.Fatalf("pgxpool.New on a syntactically valid DSN failed: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// postRaw drives HandleStripeWebhook with an arbitrary body and no valid
// signature. Every test using it asserts a rejection that happens at or before
// signature verification.
func postRaw(t *testing.T, svc *Service, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/webhooks/stripe", bytes.NewReader(body))
	r.Header.Set("Stripe-Signature", "t=1,v1=deadbeef")
	w := httptest.NewRecorder()
	svc.HandleStripeWebhook(w, r)
	return w
}

func TestWebhookRefusesWhenSecretUnset(t *testing.T) {
	t.Setenv("STRIPE_WEBHOOK_SECRET", "")
	svc := NewService()
	svc.SetSysDB(dsnFreePool(t))

	w := postRaw(t, svc, []byte(`{}`))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured webhook: got %d, want 503", w.Code)
	}
	// 503, not 500: the deployment is missing configuration, and Stripe should
	// retry rather than treat the event as permanently rejected.
	if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
}

// TestWebhookFailsClosedWithoutSysDB is the regression guard for the wiring half
// of the 2026-09-04 fix. If a future main.go drops billingSvc.SetSysDB, the handler
// must refuse the event rather than fall back to the RLS-bound pool — where the
// firms policy (init.sql, current_setting('app.current_firm') with no missing_ok)
// raises on an unset GUC.
func TestWebhookFailsClosedWithoutSysDB(t *testing.T) {
	t.Setenv("STRIPE_WEBHOOK_SECRET", "whsec_test")
	svc := NewService()
	svc.SetDB(dsnFreePool(t)) // app pool present, sys pool absent — the trap.

	w := postRaw(t, svc, []byte(`{}`))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("missing sysDB: got %d, want 500 (fail closed so Stripe retries)", w.Code)
	}
}

func TestWebhookRejectsInvalidSignature(t *testing.T) {
	t.Setenv("STRIPE_WEBHOOK_SECRET", "whsec_test")
	svc := NewService()
	svc.SetSysDB(dsnFreePool(t))

	w := postRaw(t, svc, []byte(`{"id":"evt_1","type":"customer.subscription.updated"}`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("forged signature: got %d, want 400", w.Code)
	}
}

// TestWebhookRejectsOversizedBody covers the ceiling added when this route became
// public. The body is read into memory BEFORE the HMAC can be checked — signature
// verification needs the raw bytes — so without a limit an unauthenticated caller
// can make the process allocate without bound.
func TestWebhookRejectsOversizedBody(t *testing.T) {
	t.Setenv("STRIPE_WEBHOOK_SECRET", "whsec_test")
	svc := NewService()
	svc.SetSysDB(dsnFreePool(t))

	oversized := []byte(`{"pad":"` + strings.Repeat("a", maxWebhookBody+1024) + `"}`)
	w := postRaw(t, svc, oversized)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("oversized body: got %d, want 400", w.Code)
	}
	// The distinction that matters: rejected as a bad *read*, not misreported as a
	// signature failure. MaxBytesReader errors at the ceiling instead of silently
	// truncating, which is what keeps these two causes separable in the logs.
	var problem struct {
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &problem); err != nil {
		t.Fatalf("response was not problem+json: %v (body %q)", err, w.Body.String())
	}
	if problem.Detail != "read failed" {
		t.Errorf("detail = %q, want \"read failed\" — an oversized body must not be "+
			"attributed to the signature", problem.Detail)
	}
}

func TestTierForPrice(t *testing.T) {
	for _, tc := range []struct{ price, want string }{
		{priceStarter, "starter"},
		{priceGrowth, "growth"},
		{priceScale, "scale"},
		{"price_something_else", ""},
		{"", ""},
	} {
		if got := tierForPrice(tc.price); got != tc.want {
			t.Errorf("tierForPrice(%q) = %q, want %q", tc.price, got, tc.want)
		}
	}
}

// subscriptionPayload builds a customer.subscription.* body in the shape Stripe
// actually sends. Only documented wire keys appear here — that is the whole reason
// stripeEnvelope reads the raw bytes instead of the SDK structs.
func subscriptionPayload(created int64, customer, status, priceID string,
	periodEnd int64, cancelAtPeriodEnd bool) []byte {
	return []byte(fmt.Sprintf(`{
	  "id": "evt_%d",
	  "object": "event",
	  "created": %d,
	  "type": "customer.subscription.updated",
	  "data": {
	    "object": {
	      "id": "sub_test",
	      "object": "subscription",
	      "customer": %q,
	      "status": %q,
	      "cancel_at_period_end": %t,
	      "current_period_end": %d,
	      "items": {
	        "object": "list",
	        "data": [
	          {"id": "si_test", "object": "subscription_item", "price": {"id": %q, "object": "price"}}
	        ]
	      }
	    }
	  }
	}`, created, created, customer, status, cancelAtPeriodEnd, periodEnd, priceID))
}

func parseEnvelope(t *testing.T, payload []byte) *stripeEnvelope {
	t.Helper()
	var env stripeEnvelope
	if err := json.Unmarshal(payload, &env); err != nil {
		t.Fatalf("envelope parse failed: %v", err)
	}
	return &env
}

func TestStripeEnvelopeReadsDocumentedWireKeys(t *testing.T) {
	const periodEnd = 1793000000
	env := parseEnvelope(t, subscriptionPayload(1790000000, "cus_abc", "past_due",
		priceGrowth, periodEnd, true))

	if env.Created != 1790000000 {
		t.Errorf("Created = %d, want 1790000000", env.Created)
	}
	if env.Data.Object.Customer != "cus_abc" {
		t.Errorf("Customer = %q, want cus_abc", env.Data.Object.Customer)
	}
	if env.Data.Object.Status != "past_due" {
		t.Errorf("Status = %q, want past_due", env.Data.Object.Status)
	}
	if !env.Data.Object.CancelAtPeriodEnd {
		t.Error("CancelAtPeriodEnd = false, want true")
	}
	if env.Data.Object.CurrentPeriodEnd != periodEnd {
		t.Errorf("CurrentPeriodEnd = %d, want %d", env.Data.Object.CurrentPeriodEnd, periodEnd)
	}
	if got := env.priceID(); got != priceGrowth {
		t.Errorf("priceID() = %q, want %q", got, priceGrowth)
	}
	if got := tierForPrice(env.priceID()); got != "growth" {
		t.Errorf("tier = %q, want growth", got)
	}
}

// A subscription.deleted payload can arrive with no line items. priceID must
// return "" rather than panic on an empty slice — the nil-guard is the point.
func TestStripeEnvelopePriceIDWithNoItems(t *testing.T) {
	env := parseEnvelope(t, []byte(`{"id":"evt_1","created":1,"data":{"object":
		{"customer":"cus_x","status":"canceled","items":{"data":[]}}}}`))
	if got := env.priceID(); got != "" {
		t.Errorf("priceID() with no items = %q, want empty", got)
	}
}

// ---------------------------------------------------------------------------
// Database-backed tests.
//
// DATABASE_URL_TEST_OWNER is the same BYPASSRLS/owner DSN the middleware security
// suite uses for fixtures (see internal/middleware/security_test.go). It is the
// right pool here for a second reason: it is the enforcement posture the webhook
// itself runs under, so these tests exercise the real access path rather than a
// friendlier one.
// ---------------------------------------------------------------------------

func requireSysDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL_TEST_OWNER")
	if dsn == "" {
		t.Skip("DATABASE_URL_TEST_OWNER not set; skipping billing persistence tests")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Skipf("no test database available: %v", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		pool.Close()
		t.Skipf("test database not reachable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// insertFirm creates a firm and returns its id. customer may be "" to leave the
// Stripe customer unlinked. The customer id is uniquified because firms carries a
// UNIQUE partial index on stripe_customer_id.
func insertFirm(t *testing.T, pool *pgxpool.Pool, customer string) (firmID, customerID string) {
	t.Helper()
	ctx := context.Background()
	if customer != "" {
		customerID = fmt.Sprintf("%s_%d", customer, time.Now().UnixNano())
	}
	var cust *string
	if customerID != "" {
		cust = &customerID
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO firms (name, stripe_customer_id) VALUES ($1, $2) RETURNING id`,
		"billing-test-firm", cust).Scan(&firmID); err != nil {
		t.Fatalf("insert firm: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `DELETE FROM firms WHERE id = $1`, firmID); err != nil {
			t.Logf("cleanup of firm %s failed: %v", firmID, err)
		}
	})
	return firmID, customerID
}

// readSubscription returns the persisted state. Pointers so NULL is visible as
// NULL rather than collapsing into "".
func readSubscription(t *testing.T, pool *pgxpool.Pool, firmID string) (status, tier *string,
	eventAt *time.Time, cancelAtPeriodEnd bool) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), `
		SELECT subscription_status, subscription_tier, subscription_event_at,
		       subscription_cancel_at_period_end
		  FROM firms WHERE id = $1`, firmID,
	).Scan(&status, &tier, &eventAt, &cancelAtPeriodEnd); err != nil {
		t.Fatalf("read subscription state: %v", err)
	}
	return status, tier, eventAt, cancelAtPeriodEnd
}

// applyEvent runs the subscription branch and reports the status code the webhook
// would return. A recorder's Code is 200 until something writes, which matches
// HandleStripeWebhook: applySubscription returning true falls through to its final
// 200.
func applyEvent(t *testing.T, svc *Service, payload []byte) int {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/webhooks/stripe", bytes.NewReader(payload))
	w := httptest.NewRecorder()
	svc.applySubscription(r, w, parseEnvelope(t, payload))
	return w.Code
}

func TestSubscriptionStateIsPersisted(t *testing.T) {
	pool := requireSysDB(t)
	svc := NewService()
	svc.SetSysDB(pool)
	firmID, customer := insertFirm(t, pool, "cus_persist")

	if code := applyEvent(t, svc, subscriptionPayload(1790000000, customer, "active",
		priceGrowth, 1793000000, false)); code != http.StatusOK {
		t.Fatalf("apply active event: got %d, want 200", code)
	}

	status, tier, eventAt, cancelAtPeriodEnd := readSubscription(t, pool, firmID)
	if status == nil || *status != "active" {
		t.Errorf("subscription_status = %v, want active", status)
	}
	if tier == nil || *tier != "growth" {
		t.Errorf("subscription_tier = %v, want growth", tier)
	}
	if eventAt == nil || eventAt.Unix() != 1790000000 {
		t.Errorf("subscription_event_at = %v, want unix 1790000000", eventAt)
	}
	if cancelAtPeriodEnd {
		t.Error("subscription_cancel_at_period_end = true, want false")
	}
}

// TestSubscriptionOutOfOrderEventIsIgnored is the isolation test for the watermark.
// Stripe does not guarantee delivery order, so without it the last event to arrive
// wins rather than the last event to have happened — a stale "active" would
// resurrect a cancelled subscription.
//
// Asserted in both directions on purpose: an older event must be dropped AND a
// newer one must still apply. Only the first half would also pass if the UPDATE
// never ran at all, which is the failure shape this project keeps finding.
func TestSubscriptionOutOfOrderEventIsIgnored(t *testing.T) {
	pool := requireSysDB(t)
	svc := NewService()
	svc.SetSysDB(pool)
	firmID, customer := insertFirm(t, pool, "cus_order")

	if code := applyEvent(t, svc, subscriptionPayload(2000000000, customer, "canceled",
		priceGrowth, 2000000000, false)); code != http.StatusOK {
		t.Fatalf("apply newer canceled event: got %d, want 200", code)
	}
	// Older event, delivered second.
	if code := applyEvent(t, svc, subscriptionPayload(1000000000, customer, "active",
		priceGrowth, 1500000000, false)); code != http.StatusOK {
		t.Fatalf("stale event should be accepted-and-ignored (200), got %d", code)
	}
	status, _, eventAt, _ := readSubscription(t, pool, firmID)
	if status == nil || *status != "canceled" {
		t.Fatalf("stale event overwrote newer state: subscription_status = %v, want canceled", status)
	}
	if eventAt == nil || eventAt.Unix() != 2000000000 {
		t.Errorf("watermark moved backwards: %v, want unix 2000000000", eventAt)
	}

	// Other direction: a genuinely newer event must still land.
	if code := applyEvent(t, svc, subscriptionPayload(2100000000, customer, "active",
		priceScale, 2200000000, true)); code != http.StatusOK {
		t.Fatalf("apply newest event: got %d, want 200", code)
	}
	status, tier, _, cancelAtPeriodEnd := readSubscription(t, pool, firmID)
	if status == nil || *status != "active" {
		t.Errorf("newer event did not apply: subscription_status = %v, want active", status)
	}
	if tier == nil || *tier != "scale" {
		t.Errorf("subscription_tier = %v, want scale", tier)
	}
	if !cancelAtPeriodEnd {
		t.Error("subscription_cancel_at_period_end = false, want true")
	}
}

// TestSubscriptionForUnlinkedCustomerIsRetryable is the direct regression guard for
// the original bug: the handler answered 200 no matter what happened, so Stripe
// marked the delivery successful and never retried, and the firm's subscription
// state was lost permanently. 500 is the correct answer — Stripe retries, and the
// retry succeeds once checkout.session.completed writes the customer link.
func TestSubscriptionForUnlinkedCustomerIsRetryable(t *testing.T) {
	pool := requireSysDB(t)
	svc := NewService()
	svc.SetSysDB(pool)
	insertFirm(t, pool, "") // a firm exists, but with no Stripe customer linked

	unknown := fmt.Sprintf("cus_never_linked_%d", time.Now().UnixNano())
	code := applyEvent(t, svc, subscriptionPayload(1790000000, unknown, "active",
		priceStarter, 1793000000, false))
	if code != http.StatusInternalServerError {
		t.Fatalf("event for unlinked customer: got %d, want 500 so Stripe retries", code)
	}
}

// A subscription.deleted payload can omit line items. The UPDATE uses
// COALESCE($tier, subscription_tier) so a known tier survives; a naive assignment
// would blank it and the firm would look like it never had a plan.
func TestSubscriptionWithoutItemsKeepsKnownTier(t *testing.T) {
	pool := requireSysDB(t)
	svc := NewService()
	svc.SetSysDB(pool)
	firmID, customer := insertFirm(t, pool, "cus_coalesce")

	if code := applyEvent(t, svc, subscriptionPayload(1790000000, customer, "active",
		priceStarter, 1793000000, false)); code != http.StatusOK {
		t.Fatalf("seed event: got %d, want 200", code)
	}
	deleted := []byte(fmt.Sprintf(`{"id":"evt_del","created":1790001000,
		"type":"customer.subscription.deleted","data":{"object":{"customer":%q,
		"status":"canceled","cancel_at_period_end":false,"items":{"data":[]}}}}`, customer))
	if code := applyEvent(t, svc, deleted); code != http.StatusOK {
		t.Fatalf("deleted event: got %d, want 200", code)
	}

	status, tier, _, _ := readSubscription(t, pool, firmID)
	if status == nil || *status != "canceled" {
		t.Errorf("subscription_status = %v, want canceled", status)
	}
	if tier == nil || *tier != "starter" {
		t.Errorf("subscription_tier = %v, want starter to survive an itemless event", tier)
	}
}

// checkoutPayload builds a checkout.session.completed body. `customer` is a plain
// string here because that is what Stripe puts on the wire for an unexpanded
// expandable field — the reason the handler reads the envelope rather than
// stripe.CheckoutSession.Customer.
func checkoutPayload(created int64, customer, firmID string) []byte {
	return []byte(fmt.Sprintf(`{
	  "id": "evt_%d",
	  "object": "event",
	  "created": %d,
	  "type": "checkout.session.completed",
	  "data": {
	    "object": {
	      "id": "cs_test",
	      "object": "checkout.session",
	      "mode": "subscription",
	      "customer": %q,
	      "status": "complete",
	      "metadata": {"firm_id": %q}
	    }
	  }
	}`, created, created, customer, firmID))
}

func TestCheckoutLinksCustomerToFirm(t *testing.T) {
	pool := requireSysDB(t)
	svc := NewService()
	svc.SetSysDB(pool)
	firmID, _ := insertFirm(t, pool, "")

	customer := fmt.Sprintf("cus_linked_%d", time.Now().UnixNano())
	payload := checkoutPayload(1790000000, customer, firmID)
	env := parseEnvelope(t, payload)
	if got := env.Data.Object.Metadata["firm_id"]; got != firmID {
		t.Fatalf("envelope metadata firm_id = %q, want %q", got, firmID)
	}

	r := httptest.NewRequest(http.MethodPost, "/v1/webhooks/stripe", bytes.NewReader(payload))
	w := httptest.NewRecorder()
	if ok := svc.applyCheckout(r, w, firmID, env); !ok {
		t.Fatalf("applyCheckout returned false, response %d %s", w.Code, w.Body.String())
	}

	var got *string
	if err := pool.QueryRow(context.Background(),
		`SELECT stripe_customer_id FROM firms WHERE id = $1`, firmID).Scan(&got); err != nil {
		t.Fatalf("read back customer: %v", err)
	}
	if got == nil || *got != customer {
		t.Errorf("stripe_customer_id = %v, want %q", got, customer)
	}
}

// The opposite of the retryable case: an unknown firm id cannot become known on
// redelivery, so this must NOT be a 500. Answering 500 here would make Stripe retry
// a permanently-failing event for days.
func TestCheckoutForUnknownFirmIsNotRetried(t *testing.T) {
	pool := requireSysDB(t)
	svc := NewService()
	svc.SetSysDB(pool)

	const absent = "00000000-0000-0000-0000-000000000000"
	payload := checkoutPayload(1790000000, "cus_orphan", absent)
	r := httptest.NewRequest(http.MethodPost, "/v1/webhooks/stripe", bytes.NewReader(payload))
	w := httptest.NewRecorder()
	if ok := svc.applyCheckout(r, w, absent, parseEnvelope(t, payload)); ok {
		t.Fatal("applyCheckout returned true for a firm that does not exist")
	}
	if w.Code != http.StatusOK {
		t.Errorf("unknown firm: got %d, want 200 (not retryable)", w.Code)
	}
}
