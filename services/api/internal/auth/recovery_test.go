package auth

// recovery_test.go — the pure parts of the recovery-code build, no DB.
//
// GenerateRecoveryCodes' count, format, uniqueness, and cross-batch entropy;
// HashRecoveryCode's determinism, format, and a known-answer vector computed
// independently (python hashlib) — so "valid hash" cross-checks two
// implementations of the spec instead of one library agreeing with itself.
//
// The DB-backed behaviour (the atomic single-use consumption inside the
// login's transaction, the rollback on a failed login, the lockout counter
// spending) belongs in the DATABASE_URL_TEST suite and is not covered here —
// named so it is not mistaken for done.

import (
	"encoding/hex"
	"testing"
)

func TestGenerateRecoveryCodes(t *testing.T) {
	codes, err := GenerateRecoveryCodes(10)
	if err != nil {
		t.Fatalf("generation failed: %v", err)
	}
	if len(codes) != 10 {
		t.Fatalf("got %d codes, want 10", len(codes))
	}
	seen := make(map[string]struct{})
	for i, c := range codes {
		if len(c) != 16 { // 8 bytes = 64 bits = 16 hex chars
			t.Errorf("code %d: length %d, want 16 hex chars", i, len(c))
		}
		if _, err := hex.DecodeString(c); err != nil {
			t.Errorf("code %d: not valid hex: %v", i, err)
		}
		if _, dup := seen[c]; dup {
			t.Errorf("code %d: duplicate in batch", i)
		}
		seen[c] = struct{}{}
	}
}

func TestGenerateRecoveryCodes_BatchesDiffer(t *testing.T) {
	a, err := GenerateRecoveryCodes(10)
	if err != nil {
		t.Fatalf("generation failed: %v", err)
	}
	b, err := GenerateRecoveryCodes(10)
	if err != nil {
		t.Fatalf("generation failed: %v", err)
	}
	same := 0
	for _, x := range a {
		for _, y := range b {
			if x == y {
				same++
			}
		}
	}
	if same != 0 {
		t.Errorf("%d code(s) appeared in both batches — the entropy is broken", same)
	}
}

func TestHashRecoveryCode_KnownAnswerAndFormat(t *testing.T) {
	const want = "bac60529cfdeb5b9384bbedb66d8081c4a7b6addf78612c413ffe97fd2e6fa8e" // sha256("test-code-1234"), computed independently
	got := HashRecoveryCode("test-code-1234")
	if got != want {
		t.Fatalf("hash = %q, want the known-answer %q — the hashing drifted from sha256", got, want)
	}
	if again := HashRecoveryCode("test-code-1234"); again != got {
		t.Fatalf("hashing is not deterministic: %q vs %q", got, again)
	}
	if len(got) != 64 {
		t.Fatalf("hash length %d, want 64 (sha256 hex)", len(got))
	}
	if HashRecoveryCode("a") == HashRecoveryCode("b") {
		t.Fatal("different codes hashed identically")
	}
	// The stored form never round-trips: hashing the hash is not the code.
	if HashRecoveryCode(HashRecoveryCode("test-code-1234")) == want {
		t.Fatal("hash of hash equals the code hash — the stored form is reversible, which it must not be")
	}
}
