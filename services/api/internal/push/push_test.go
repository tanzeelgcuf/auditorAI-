package push

import (
	"encoding/json"
	"testing"
)

// TestSeverityShouldNotify gates only high severity.
func TestSeverityShouldNotify(t *testing.T) {
	if !severityShouldNotify("high") {
		t.Fatal("high severity should notify")
	}
	for _, s := range []string{"info", "low", "medium", "", "HIGH"} {
		if severityShouldNotify(s) {
			t.Fatalf("severity %q should NOT notify", s)
		}
	}
}

// TestValidToken accepts valid Expo tokens, rejects malformed ones.
func TestValidToken(t *testing.T) {
	valid := "ExponentPushToken[abcdefghijklmnopqrstuvwxyz123456]"
	if !validToken(valid) {
		t.Fatal("valid token rejected")
	}
	for _, bad := range []string{"", "short", "ExponentPushToken[" + string(rune(32)) + "]", "ExponentPushToken[has space in it]"} {
		if validToken(bad) {
			t.Fatalf("invalid token %q accepted", bad)
		}
	}
}

// TestBuildExpoPayload verifies the payload shape for multiple tokens.
func TestBuildExpoPayload(t *testing.T) {
	tokens := []string{"tok1", "tok2"}
	body := BuildExpoPayload(tokens, "finding-1", "book-9", "high", "GL mismatch $50")

	var msgs []map[string]interface{}
	if err := json.Unmarshal(body, &msgs); err != nil {
		t.Fatalf("payload not valid JSON: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	if msgs[0]["to"] != "tok1" || msgs[1]["to"] != "tok2" {
		t.Fatalf("to field not per-token: %v", msgs)
	}
	if msgs[0]["title"] != "High-severity finding" {
		t.Fatalf("title = %v", msgs[0]["title"])
	}
	data := msgs[0]["data"].(map[string]interface{})
	if data["finding_id"] != "finding-1" || data["book_id"] != "book-9" {
		t.Fatalf("data = %v", data)
	}
}

// TestBuildExpoPayloadEmpty produces a valid empty array.
func TestBuildExpoPayloadEmpty(t *testing.T) {
	body := BuildExpoPayload(nil, "f", "b", "high", "s")
	if string(body) != "[]" {
		t.Fatalf("empty tokens should yield [], got %s", body)
	}
}
