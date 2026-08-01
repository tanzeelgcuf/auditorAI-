package portal

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestGetPortalBookID round-trips the context value.
func TestGetPortalBookID(t *testing.T) {
	ctx := context.WithValue(context.Background(), portalBookKey, "book-1")
	if got := GetPortalBookID(ctx); got != "book-1" {
		t.Fatalf("GetPortalBookID = %q, want book-1", got)
	}
	if got := GetPortalBookID(context.Background()); got != "" {
		t.Fatalf("GetPortalBookID(empty) = %q, want empty", got)
	}
}

// TestBookGuardMismatch verifies a foreign book id returns 404 via the guard.
func TestBookGuardMismatch(t *testing.T) {
	ctx := context.WithValue(context.Background(), portalBookKey, "book-1")
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)

	ok := bookGuard(w, req.Context(), "book-2")
	if ok {
		t.Fatal("bookGuard should reject a mismatched resource book")
	}
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

// TestBookGuardMatch verifies the same book passes without writing a response.
func TestBookGuardMatch(t *testing.T) {
	ctx := context.WithValue(context.Background(), portalBookKey, "book-1")
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)

	if ok := bookGuard(w, req.Context(), "book-1"); !ok {
		t.Fatal("bookGuard should pass for the scoped book")
	}
	// Response must remain untouched on a match (still 200 default from recorder,
	// body empty — no 404 problem+json written).
	if w.Body.Len() != 0 {
		t.Fatalf("bookGuard should not write a body on match, got %q", w.Body.String())
	}
}
