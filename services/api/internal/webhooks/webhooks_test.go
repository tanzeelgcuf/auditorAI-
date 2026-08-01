package webhooks

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestSignKnownAnswer verifies HMAC-SHA256 hex output. Expected value computed
// independently: HMAC-SHA256(key="secret", body="sample body") hex.
func TestSignKnownAnswer(t *testing.T) {
	got := sign("secret", []byte("sample body"))
	const want = "99132f7d3727d4fe9c8ef59e793e15bc3332f018a70caf8f403bb6535a73cdd5"
	if got != want {
		t.Fatalf("sign() = %s, want %s", got, want)
	}
}

// TestSignEmptyBody and TestSignDifferentKey ensure sensitivity.
func TestSignSensitivity(t *testing.T) {
	a := sign("secret", []byte("body"))
	b := sign("secret", []byte("body"))
	c := sign("other", []byte("body"))
	if a != b {
		t.Fatal("same inputs must produce same signature")
	}
	if a == c {
		t.Fatal("different key must produce different signature")
	}
}

// TestDeliverPostsSignedBody verifies deliver POSTs the right body + header.
func TestDeliverPostsSignedBody(t *testing.T) {
	var gotSignature, gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSignature = r.Header.Get("X-AI-Auditor-Signature")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	svc := &Service{}
	sub := &Subscription{TargetURL: server.URL, SigningSecret: "secret"}
	err := svc.deliver(context.Background(), sub, "finding.created",
		map[string]any{"finding_id": "f1", "severity": "high"})
	if err != nil {
		t.Fatalf("deliver failed: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal([]byte(gotBody), &payload); err != nil {
		t.Fatalf("bad JSON body: %v", err)
	}
	if payload["event"] != "finding.created" {
		t.Errorf("event = %v, want finding.created", payload["event"])
	}
	data := payload["data"].(map[string]any)
	if data["finding_id"] != "f1" {
		t.Errorf("finding_id = %v, want f1", data["finding_id"])
	}
	if gotSignature != sign("secret", []byte(gotBody)) {
		t.Errorf("signature header does not match body HMAC")
	}
}

// TestDeliverRetryOn5xx verifies a 500-then-200 server succeeds on retry.
func TestDeliverRetryOn5xx(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	svc := &Service{}
	sub := &Subscription{TargetURL: server.URL, SigningSecret: "secret"}
	err := svc.deliver(context.Background(), sub, "finding.created",
		map[string]any{"finding_id": "f1"})
	if err != nil {
		t.Fatalf("deliver should succeed after retry, got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got < 2 {
		t.Fatalf("expected retry (>=2 calls), got %d", got)
	}
}

// TestDeliverNoRetryOn4xx verifies 4xx is not retried.
func TestDeliverNoRetryOn4xx(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()

	svc := &Service{}
	sub := &Subscription{TargetURL: server.URL, SigningSecret: "secret"}
	err := svc.deliver(context.Background(), sub, "finding.created", map[string]any{})
	if err == nil {
		t.Fatal("4xx should fail delivery")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("4xx should not retry, got %d calls", got)
	}
}

// TestDeliverContextTimeout ensures a hung target respects ctx timeout.
func TestDeliverContextTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Second)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	svc := &Service{}
	sub := &Subscription{TargetURL: server.URL, SigningSecret: "secret"}
	start := time.Now()
	err := svc.deliver(ctx, sub, "finding.created", map[string]any{})
	if err == nil {
		t.Fatal("hung target should time out")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("deliver should respect ctx timeout, took %v", time.Since(start))
	}
}
