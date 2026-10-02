// Package connectors integrates accounting providers (QuickBooks Online,
// Xero) into the pipeline. The connector is another ingestion SOURCE, not a
// parallel pipeline: it reads structured data from the provider's API, maps
// it into the same entity shapes ingestion produces, writes them into the
// same tables, and publishes the same pipeline events — so the downstream
// extraction, linking, and verification are unchanged.
package connectors

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
)

// encryptToken/decryptToken: AES-256-GCM under CONNECTOR_ENC_KEY (a 32-byte
// hex key). The research's requirement: refresh tokens are stored ENCRYPTED —
// they are long-lived (Intuit expires them after 100 days of inactivity) and
// must not sit in plaintext. A missing key FAILS the operation, not the
// request's other work: a connection whose tokens cannot be decrypted is
// unusable and must surface as an error, not silently re-authenticate.
//
// NOT done here, named so it is not mistaken for covered: per-tenant KMS keys
// (the SOC2 white row). The env key is a dev posture; production moves it
// behind KMS.
func encryptionKey() ([]byte, error) {
	v := os.Getenv("CONNECTOR_ENC_KEY")
	if v == "" {
		return nil, errors.New("CONNECTOR_ENC_KEY not set: connector tokens cannot be encrypted")
	}
	key, err := hex.DecodeString(v)
	if err != nil {
		return nil, errors.New("CONNECTOR_ENC_KEY is not valid hex")
	}
	if len(key) != 32 {
		return nil, errors.New("CONNECTOR_ENC_KEY must decode to 32 bytes (AES-256)")
	}
	return key, nil
}

func encryptToken(plaintext string) (string, error) {
	key, err := encryptionKey()
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	out := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return hex.EncodeToString(out), nil
}

func decryptToken(encoded string) (string, error) {
	key, err := encryptionKey()
	if err != nil {
		return "", err
	}
	raw, err := hex.DecodeString(encoded)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("ciphertext too short")
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}
