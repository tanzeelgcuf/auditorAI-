package connectors

// crypto_test.go — the token encryption, pure, no DB, no network.
//
// The round trip, the nonce uniqueness (GCM's nonce must never repeat for the
// same key), the GCM authentication (a tampered ciphertext must fail), and
// the key validation (a missing/invalid key fails the operation — a
// connection whose tokens cannot be encrypted must surface as an error, not
// silently proceed).

import (
	"strings"
	"testing"
)

const testKey = "6f2a1c0d9e8b7a4c3d2e1f0a9b8c7d6e5f4a3b2c1d0e9f8a7b6c5d4e3f2a1b0c" // 64 hex = 32 bytes

func TestEncryptDecryptRoundTrip(t *testing.T) {
	t.Setenv("CONNECTOR_ENC_KEY", testKey)
	plain := "refresh-token-value-1234"
	enc, err := encryptToken(plain)
	if err != nil {
		t.Fatalf("encrypt failed: %v", err)
	}
	if enc == plain {
		t.Fatal("ciphertext equals plaintext — nothing was encrypted")
	}
	dec, err := decryptToken(enc)
	if err != nil {
		t.Fatalf("decrypt failed: %v", err)
	}
	if dec != plain {
		t.Fatalf("round trip: got %q, want %q", dec, plain)
	}
}

func TestEncrypt_NonceUniquePerCall(t *testing.T) {
	t.Setenv("CONNECTOR_ENC_KEY", testKey)
	a, err := encryptToken("same-plaintext")
	if err != nil {
		t.Fatalf("encrypt failed: %v", err)
	}
	b, err := encryptToken("same-plaintext")
	if err != nil {
		t.Fatalf("encrypt failed: %v", err)
	}
	if a == b {
		t.Fatal("two encryptions of the same plaintext produced identical ciphertext — the nonce is not random")
	}
}

func TestDecrypt_TamperedCiphertextFails(t *testing.T) {
	t.Setenv("CONNECTOR_ENC_KEY", testKey)
	enc, err := encryptToken("refresh-token-value-1234")
	if err != nil {
		t.Fatalf("encrypt failed: %v", err)
	}
	b := []byte(enc)
	b[len(b)-3] ^= 0x01 // flip one bit of the ciphertext body
	if _, err := decryptToken(string(b)); err == nil {
		t.Fatal("tampered ciphertext decrypted without error — GCM authentication is not enforced")
	}
}

func TestKeyValidation(t *testing.T) {
	t.Setenv("CONNECTOR_ENC_KEY", "")
	if _, err := encryptToken("x"); err == nil || !strings.Contains(err.Error(), "not set") {
		t.Errorf("missing key: err %v, want a 'not set' failure", err)
	}
	t.Setenv("CONNECTOR_ENC_KEY", "zz-not-hex")
	if _, err := encryptToken("x"); err == nil || !strings.Contains(err.Error(), "hex") {
		t.Errorf("non-hex key: err %v, want a hex failure", err)
	}
	t.Setenv("CONNECTOR_ENC_KEY", "abcd")
	if _, err := encryptToken("x"); err == nil || !strings.Contains(err.Error(), "32 bytes") {
		t.Errorf("short key: err %v, want a 32-bytes failure", err)
	}
}
