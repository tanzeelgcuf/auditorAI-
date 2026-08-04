// InternalAuth tests — internal key auth for MCP tools (doc 05 §3).
package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// A functional test needs a real DB pool; that runs in integration. These
// cover the structural + env behavior that needs no DB.

func TestInternalAuthRequiresKeyHeader(t *testing.T) {
	t.Setenv("API_INTERNAL_KEY", "test-secret")
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	// nil pool is fine — the missing-header path rejects before any DB use.
	h := InternalAuth(nil)(next)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mcp/tools/x", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("missing X-Internal-Key: got %d, want 401", rec.Code)
	}
}

func TestInternalAuthRejectsWrongKey(t *testing.T) {
	t.Setenv("API_INTERNAL_KEY", "test-secret")
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := InternalAuth(nil)(next)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mcp/tools/x", nil)
	req.Header.Set("X-Internal-Key", "wrong")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong key: got %d, want 401", rec.Code)
	}
}

func TestInternalAuthDisabledWhenNoKeyConfigured(t *testing.T) {
	t.Setenv("API_INTERNAL_KEY", "")
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := InternalAuth(nil)(next)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mcp/tools/x", nil)
	req.Header.Set("X-Internal-Key", "anything")
	h.ServeHTTP(rec, req)
	// With no configured key, every request is rejected (fail closed).
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no configured key: got %d, want 401 (fail closed)", rec.Code)
	}
}

// Body-restore + book resolution need a real DB pool (integration-tested in the
// live stress run). The structural tests above cover the fail-closed paths.