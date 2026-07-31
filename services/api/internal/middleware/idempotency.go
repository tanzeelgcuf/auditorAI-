package middleware

// Idempotency — doc 06 §4. Clients send an Idempotency-Key header on uploads and
// report generation; a retry with the same key returns the cached response instead
// of reprocessing. Cache lives in Postgres (idempotency_keys table), TTL 24h.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const idempotencyTTL = 24 * time.Hour

// Idempotency middleware. For requests with an Idempotency-Key:
//   - if a prior response exists for (key, user): return it (200) without reprocessing
//   - otherwise stamp the request with the key and let the handler store its response
//     via StoreIdempotentResponse.
func Idempotency(db *pgxpool.Pool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get("Idempotency-Key")
			if key == "" {
				next.ServeHTTP(w, r)
				return
			}

			userID := GetUserID(r.Context())
			keyHash := hashKey(userID + ":" + key)

			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()

			var cached []byte
			err := db.QueryRow(ctx,
				`SELECT response_body FROM idempotency_keys
				 WHERE key_hash = $1 AND created_at > now() - $2::interval`,
				keyHash, idempotencyTTL.String()).Scan(&cached)
			if err == nil {
				// Replay cached response
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				w.Write(cached)
				return
			}
			if err != pgx.ErrNoRows {
				// DB error — fail closed on the idempotency path is unsafe (blocks retry
				// semantics), so fall through and let the handler proceed.
				next.ServeHTTP(w, r)
				return
			}

			// Stamp the key into context; handler calls StoreIdempotentResponse.
			rec := &idempotencyRecorder{ResponseWriter: w, keyHash: keyHash, db: db, userID: userID}
			next.ServeHTTP(rec, r)
		})
	}
}

func hashKey(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

type idempotencyRecorder struct {
	http.ResponseWriter
	db      *pgxpool.Pool
	keyHash string
	userID  string
	status  int
	body    []byte
	done    bool
}

func (r *idempotencyRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *idempotencyRecorder) Write(b []byte) (int, error) {
	if !r.done {
		r.body = append(r.body, b...)
	}
	return r.ResponseWriter.Write(b)
}

// StoreIdempotentResponse is called by handlers after a successful (2xx) write
// so a retry with the same key replays the same response.
func StoreIdempotentResponse(ctx context.Context, db *pgxpool.Pool, userID, key string, status int, body []byte) {
	if key == "" || status < 200 || status >= 300 {
		return
	}
	keyHash := hashKey(userID + ":" + key)
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := db.Exec(cctx,
		`INSERT INTO idempotency_keys (key_hash, user_id, response_status, response_body)
		 VALUES ($1, $2, $3, $4) ON CONFLICT (key_hash) DO NOTHING`,
		keyHash, userID, status, body)
	if err != nil {
		// Non-fatal: a failed idempotency store just means the retry reprocesses.
		_ = err
	}
}

// EncodeJSON is a helper for handlers to produce a body and store it.
func EncodeJSON(v interface{}) ([]byte, error) {
	return json.Marshal(v)
}
