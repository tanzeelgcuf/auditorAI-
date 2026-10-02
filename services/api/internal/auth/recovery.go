// services/api/internal/auth/recovery.go
//
// Recovery codes: the account's ALTERNATIVE second factor, submitted in place
// of a TOTP code at login. Until 2026-10-01 a user who lost their
// authenticator was locked out and needed an operator to clear totp_secret —
// named as a gap in totp.go's header and SOC2_READINESS.md CC6.5.
//
// The research's model, applied (GitHub's is the canonical one): hashed at
// rest (SHA-256 — recovery codes are high-entropy random values and need no
// key-stretching, unlike passwords, which are low-entropy and get Argon2id),
// 16 hex chars = 64 bits of entropy, 10 codes per batch, single-use enforced
// by an atomic UPDATE inside the login's transaction, the plaintext shown
// exactly once at generation and never retrievable, and regenerating
// invalidates the old batch in the same transaction that stores the new one.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
)

// recoveryCodeCount is the batch size: single-use means a code works once, so
// the user needs enough to survive losing the authenticator.
const recoveryCodeCount = 10

// GenerateRecoveryCodes returns n fresh recovery codes as plaintext. The
// caller stores HashRecoveryCode(code) and shows the plaintext exactly once;
// the database never sees it. Duplicates within a batch are skipped (the
// schema's UNIQUE (user_id, code_hash) would otherwise fail the insert).
func GenerateRecoveryCodes(n int) ([]string, error) {
	codes := make([]string, 0, n)
	seen := make(map[string]struct{}, n)
	for len(codes) < n {
		buf := make([]byte, 8) // 64 bits of entropy
		if _, err := rand.Read(buf); err != nil {
			return nil, err
		}
		code := hex.EncodeToString(buf)
		if _, dup := seen[code]; dup {
			continue
		}
		seen[code] = struct{}{}
		codes = append(codes, code)
	}
	return codes, nil
}

// HashRecoveryCode is the only form the database ever sees. Deterministic
// SHA-256 — no salt, because the input is already a 64-bit random value and a
// rainbow table against it is not cheaper than the hashing.
func HashRecoveryCode(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}
