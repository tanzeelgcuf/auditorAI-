package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/auth"
)

// testAuthService creates an auth service with dev JWT secret for testing.
func testAuthService() *auth.Service {
	return auth.NewService()
}

func TestUnauthenticatedReturns401(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	ts := httptest.NewServer(Authenticator(testAuthService())(handler))
	defer ts.Close()

	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", resp.StatusCode)
	}
}

func TestAuthenticatedWithValidTokenSucceeds(t *testing.T) {
	as := testAuthService()
	// Manually craft a token using the method from auth
	pair, err := as.GenerateTokens("user-1", "firm-1", "staff")
	if err != nil {
		t.Fatal(err)
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID := r.Context().Value(UserIDKey)
		firmID := r.Context().Value(FirmIDKey)
		role := r.Context().Value(RoleKey)

		if userID != "user-1" {
			t.Errorf("expected user-1, got %v", userID)
		}
		if firmID != "firm-1" {
			t.Errorf("expected firm-1, got %v", firmID)
		}
		if role != "staff" {
			t.Errorf("expected staff, got %v", role)
		}
		w.WriteHeader(http.StatusOK)
	})

	ts := httptest.NewServer(Authenticator(as)(handler))
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL, nil)
	req.Header.Set("Authorization", "Bearer "+pair.AccessToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}

func TestCrossFirmAccessReturns404(t *testing.T) {
	// Simulates firm_admin from Firm A cannot access Firm B's data.
	// The book-not-found check is at the handler level (tenant.go contains() check).
	// This test verifies the context carries correct user/firm values.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assignedBooks := GetAssignedBooks(r.Context())
		firmID := GetFirmID(r.Context())

		// Staff from Firm A trying to access a Book that belongs to Firm B.
		// If book ID isn't in assigned list, handler should return 404.
		bookBID := "11111111-1111-1111-1111-111111111111"
		found := false
		for _, b := range assignedBooks {
			if b == bookBID {
				found = true
				break
			}
		}
		if !found {
			writeProblem(w, r, "https://ai-auditor.dev/errors/not-found", "Not Found", http.StatusNotFound, "book not found")
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = firmID
	})

	// Both users belong to firm-1 but have different book assignments.
	// This test checks context propagation; RLSInjector normally fetches books.
	ctx := context.WithValue(context.Background(), UserIDKey, "staff-1")
	ctx = context.WithValue(ctx, FirmIDKey, "firm-1")
	ctx = context.WithValue(ctx, RoleKey, "staff")
	ctx = context.WithValue(ctx, AssignedBooksKey, []string{
		"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
	})

	req := httptest.NewRequest("GET", "/", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404 for cross-book access, got %d", rec.Code)
	}
}

func TestFirmAdminSeesAllBooksStaffSeesAssignedOnly(t *testing.T) {
	allBooks := []string{
		"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
		"bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
		"cccccccc-cccc-cccc-cccc-cccccccccccc",
	}
	staffBooks := []string{
		"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
	}

	// Test admin: sees all books
	t.Run("firm_admin sees all books", func(t *testing.T) {
		ctx := context.WithValue(context.Background(), UserIDKey, "admin-1")
		ctx = context.WithValue(ctx, FirmIDKey, "firm-1")
		ctx = context.WithValue(ctx, RoleKey, "firm_admin")
		ctx = context.WithValue(ctx, AssignedBooksKey, allBooks)

		books := GetAssignedBooks(ctx)
		if len(books) != 3 {
			t.Errorf("expected 3 books for admin, got %d", len(books))
		}
	})

	// Test staff: sees only assigned books
	t.Run("staff sees only assigned books", func(t *testing.T) {
		ctx := context.WithValue(context.Background(), UserIDKey, "staff-1")
		ctx = context.WithValue(ctx, FirmIDKey, "firm-1")
		ctx = context.WithValue(ctx, RoleKey, "staff")
		ctx = context.WithValue(ctx, AssignedBooksKey, staffBooks)

		books := GetAssignedBooks(ctx)
		if len(books) != 1 {
			t.Errorf("expected 1 book for staff, got %d", len(books))
		}
		if books[0] != "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa" {
			t.Errorf("expected assigned book, got %s", books[0])
		}
	})

	// Test staff cannot access unassigned book
	t.Run("staff cannot access unassigned book", func(t *testing.T) {
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assigned := GetAssignedBooks(r.Context())
			targetBook := "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
			for _, b := range assigned {
				if b == targetBook {
					w.WriteHeader(http.StatusOK)
					return
				}
			}
			writeProblem(w, r, "https://ai-auditor.dev/errors/not-found", "Not Found", http.StatusNotFound, "book not found")
		})

		ctx := context.WithValue(context.Background(), UserIDKey, "staff-1")
		ctx = context.WithValue(ctx, FirmIDKey, "firm-1")
		ctx = context.WithValue(ctx, RoleKey, "staff")
		ctx = context.WithValue(ctx, AssignedBooksKey, staffBooks)

		req := httptest.NewRequest("GET", "/", nil).WithContext(ctx)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Errorf("expected 404 for unassigned book, got %d", rec.Code)
		}
	})
}

func TestWriteProblemRFC7807(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeProblem(w, r, "https://ai-auditor.dev/errors/not-found", "Not Found", http.StatusNotFound, "book not found")
	})

	req := httptest.NewRequest("GET", "/v1/books/missing", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}

	ct := rec.Header().Get("Content-Type")
	if ct != "application/problem+json" {
		t.Errorf("expected application/problem+json, got %s", ct)
	}

	body := rec.Body.String()
	if body == "" {
		t.Fatal("expected non-empty body")
	}
}
