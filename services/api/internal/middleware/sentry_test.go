package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	chimiddleware "github.com/go-chi/chi/v5/middleware"
)

// TestSentryRecovererRePanics proves the wrapper re-panics (so chi's Recoverer
// still renders the 500) and does not crash when no DSN is configured (events
// drop via the SDK's noop transport / nil client).
func TestSentryRecovererRePanics(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})

	// Without chi's Recoverer, the panic must re-propagate.
	func() {
		defer func() {
			if v := recover(); v == nil {
				t.Fatal("SentryRecoverer swallowed the panic — it must re-panic so chi's Recoverer renders the 500")
			} else if v != "boom" {
				t.Fatalf("unexpected recovered value: %v", v)
			}
		}()
		SentryRecoverer(inner).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/panic", nil))
	}()

	// With chi's Recoverer mounted after, the request returns 500.
	rec := httptest.NewRecorder()
	chimiddleware.Recoverer(SentryRecoverer(inner)).ServeHTTP(rec, httptest.NewRequest("GET", "/panic", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
}
