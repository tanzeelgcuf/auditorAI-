package tenant

import (
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/middleware"
)

// HandleRotateKeys (POST /v1/admin/rotate-keys, firm_admin) rotates the firm's
// data encryption key: a new key reference is activated and prior active keys
// are moved to 'rotating' so ciphertext can be re-encrypted before retirement
// (doc 05 §5). The key_ref is a storage reference (KMS id / envelope key path);
// the key material itself never passes through the API.
func (s *Service) HandleRotateKeys(w http.ResponseWriter, r *http.Request) {
	firmID := middleware.GetFirmID(r.Context())
	if firmID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	conn := middleware.GetConn(r.Context())
	db := conn
	if db == nil {
		c, err := s.db.Acquire(r.Context())
		if err != nil {
			slog.Error("failed to acquire connection", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		defer c.Release()
		db = c
	}

	keyID := uuid.NewString()
	keyRef := "kms:ai-auditor/" + firmID + "/data/" + keyID

	tx, err := db.Begin(r.Context())
	if err != nil {
		slog.Error("failed to begin key rotation", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	defer tx.Rollback(r.Context())

	// Mark all currently active keys for rotation (still valid for decrypting
	// existing ciphertext until re-encryption completes).
	_, err = tx.Exec(r.Context(),
		`UPDATE data_encryption_keys SET status = 'rotating' WHERE firm_id = $1 AND status = 'active'`,
		firmID)
	if err != nil {
		slog.Error("failed to mark prior keys for rotation", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	var id string
	err = tx.QueryRow(r.Context(),
		`INSERT INTO data_encryption_keys (firm_id, key_ref) VALUES ($1, $2) RETURNING id::text`,
		firmID, keyRef).Scan(&id)
	if err != nil {
		slog.Error("failed to insert new data encryption key", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	if err := tx.Commit(r.Context()); err != nil {
		slog.Error("failed to commit key rotation", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	slog.Info("data encryption key rotated", "firm_id", firmID, "key_id", id)
	writeJSON(w, http.StatusCreated, map[string]string{
		"id": id, "key_ref": keyRef, "status": "active",
	})
}
