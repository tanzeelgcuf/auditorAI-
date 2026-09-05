package middleware

// Idempotency — clients send an Idempotency-Key header on uploads and report
// generation; a retry with the same key replays the cached response instead of
// reprocessing. The cache lives in Postgres (idempotency_keys), TTL 24h.
//
// THE CACHE KEY IS RESOLVED IN EXACTLY ONE PLACE. resolveKeyHash runs in the
// middleware and stashes its answer in the request context; StoreIdempotentResponse
// reads that one answer. Before 2026-09-05 the middleware and each handler called
// hashKey(userID + ":" + key) independently — two resolutions of the same identity,
// which is the shape rule 13 exists to prevent. Here a disagreement between them is
// not a security hole but a silent feature death: the handler stores a row under a
// hash the middleware will never look up, so every retry reprocesses forever and
// nothing logs.
//
// The key is scoped to (firm, book, user, client key). firm and book were added when
// idempotency_keys gained RLS — without them in the hash the new columns would be
// decorative, and worse, one user replaying a key across two books would receive
// book A's response for a book B request.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const idempotencyTTL = 24 * time.Hour

// idemCtxKey is unexported and struct-typed so nothing outside this file can
// construct it — the untyped-string context key that made both TOTP handlers 401
// is the precedent for not taking the shortcut here.
type idemCtxKey struct{}

// idemScope is the single resolved answer for one request.
type idemScope struct {
	keyHash string
	firmID  string
	bookID  string // "" when the route carries no {bookId}
	userID  string
}

// ErrIdempotencyUnscoped means the request reached the idempotency layer without a
// firm or user in context, which can only happen if Idempotency is mounted ABOVE
// Authenticator. Returned rather than swallowed so the mis-mount is visible.
var ErrIdempotencyUnscoped = errors.New("idempotency: no firm/user in context (mounted above Authenticator?)")

// resolveKeyHash is the one resolution point. It returns nil when the request
// carries no Idempotency-Key (the feature is opt-in per request), and an error when
// the request is unscoped.
func resolveKeyHash(r *http.Request) (*idemScope, error) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		return nil, nil
	}
	firmID := GetFirmID(r.Context())
	userID := GetUserID(r.Context())
	if firmID == "" || userID == "" {
		return nil, ErrIdempotencyUnscoped
	}
	// chi.URLParam, not r.PathValue: this repo pins chi v5.1.0, where PathValue is
	// not populated by chi's router.
	bookID := chi.URLParam(r, "bookId")
	return &idemScope{
		keyHash: hashKey(firmID + ":" + bookID + ":" + userID + ":" + key),
		firmID:  firmID,
		bookID:  bookID,
		userID:  userID,
	}, nil
}

func hashKey(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// Idempotency replays a cached response for a repeated Idempotency-Key.
//
// Both statements run on DB(ctx, db) — the RLS-primed connection RLSInjector put in
// context — not on the raw pool. idempotency_keys carries RLS as of 2026-09-05, and
// every policy in this schema calls current_setting('app.…') with no missing_ok and
// there is no GUC default, so a raw-pool statement against this table cannot
// succeed: it either raises or tests against '' and fails the policy. Adding RLS
// while these statements were still on the raw pool would have converted a working
// feature into a silently broken one, which is why the pool fix landed first.
func Idempotency(db *pgxpool.Pool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			scope, err := resolveKeyHash(r)
			if err != nil {
				// Fail closed on a mis-mount: without a firm we would hash into a
				// shared namespace, which is a cross-tenant replay, not a cache miss.
				slog.Error("idempotency scope unresolved", "error", err, "path", r.URL.Path)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			if scope == nil {
				next.ServeHTTP(w, r)
				return
			}

			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()

			var status int
			var cached []byte
			// $2::double precision * interval '1 second', not $2::interval with a Go
			// duration string: time.Duration.String() emits "24h0m0s" and whether
			// Postgres accepts that literal is not something this environment can test.
			// An explicit numeric multiply removes the dependency on Go's format.
			// (make_interval(secs => $2) would also work but check_schema_drift reads
			// the named-argument `secs =>` as a column of the FROM relation.)
			qerr := DB(ctx, db).QueryRow(ctx,
				`SELECT response_status, response_body
				   FROM idempotency_keys
				  WHERE key_hash = $1
				    AND created_at > now() - ($2::double precision * interval '1 second')`,
				scope.keyHash, idempotencyTTL.Seconds()).Scan(&status, &cached)

			switch {
			case qerr == nil:
				// Replay the ORIGINAL status. Hardcoding 200 here made every replayed
				// upload and report answer 200 to a request whose first response was
				// 201, so a client that branches on 201 took the wrong branch on retry.
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = w.Write(cached)
				return
			case errors.Is(qerr, pgx.ErrNoRows):
				// Genuine cache miss: proceed.
			default:
				// Fail OPEN on a DB read error, and the two mounts do not carry equal risk.
				//   /documents: safe. source_documents has a real backstop —
				//     idx_unique_doc_per_book UNIQUE (client_book_id, content_hash) WHERE
				//     deleted_at IS NULL (init.sql:185), plus an explicit pre-check at
				//     documents.go:152. A reprocessed upload cannot duplicate a document.
				//   /reports: NOT backstopped. audit_reports (init.sql:290) has no unique
				//     constraint on (client_book_id, period_start, period_end), so a
				//     reprocessed generate can write a second report row and a second PDF.
				// Fail-open is still the right default — a 500 on a cache outage would take
				// the endpoint down — but the reports asymmetry is a real, logged gap, not a
				// solved problem. Deliberately not "fixed" here by adding a unique index:
				// regenerating a report for the same period may be legitimate, and that is a
				// product decision, not a middleware one.
				slog.Warn("idempotency lookup failed, processing without cache",
					"error", qerr, "path", r.URL.Path)
			}

			r = r.WithContext(context.WithValue(r.Context(), idemCtxKey{}, scope))
			next.ServeHTTP(w, r)
		})
	}
}

// StoreIdempotentResponse caches a successful response under the hash the middleware
// already resolved. It reads that hash from context rather than recomputing it, so
// there is exactly one definition of the cache key in the process.
//
// It returns an error instead of discarding one. The previous `_ = err` meant a
// permanently failing INSERT — a missing column, an RLS denial, a serialization
// failure — looked identical to success, and the only symptom was that retries kept
// reprocessing. Callers cannot change the response (it has already been written by
// the time they call this), but they can and must log.
//
// On r.Context(): this write IS cancellable by the client, unlike the audit writes
// that had to move to context.WithoutCancel. The asymmetry is deliberate — a caller
// who aborts to dodge an audit row erases evidence about themselves, whereas a caller
// who aborts here only forfeits their own replay protection. No incentive to do it,
// no integrity loss if they do.
func StoreIdempotentResponse(ctx context.Context, db Querier, status int, body []byte) error {
	scope, ok := ctx.Value(idemCtxKey{}).(*idemScope)
	if !ok || scope == nil {
		// No key on the request, or the middleware is not mounted on this route.
		// Not an error: idempotency is opt-in per request.
		return nil
	}
	if status < 200 || status >= 300 {
		// Never cache a failure — a retry after a 500 must be allowed to succeed.
		return nil
	}
	if !json.Valid(body) {
		// response_body is JSONB; a non-JSON body would raise inside Postgres with a
		// message that points at the column, not at the handler that produced it.
		return errors.New("idempotency: response body is not valid JSON")
	}

	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	// ON CONFLICT DO UPDATE ... WHERE expired, not DO NOTHING. With DO NOTHING a key
	// whose row had aged past the TTL could never be cached again: the SELECT filters
	// on created_at so it saw no row, the INSERT hit the primary key and did nothing,
	// and the request reprocessed on every retry forever while the row stayed in the
	// table. The WHERE clause keeps a LIVE row immutable — a second concurrent request
	// with the same key must not overwrite the response the first one recorded.
	_, err := DB(cctx, db).Exec(cctx,
		`INSERT INTO idempotency_keys
		     (key_hash, firm_id, client_book_id, user_id, response_status, response_body)
		 VALUES ($1, $2::uuid, NULLIF($3, '')::uuid, $4::uuid, $5, $6)
		 ON CONFLICT (key_hash) DO UPDATE
		    SET response_status = EXCLUDED.response_status,
		        response_body   = EXCLUDED.response_body,
		        created_at      = now()
		  WHERE idempotency_keys.created_at <= now() - ($7::double precision * interval '1 second')`,
		scope.keyHash, scope.firmID, scope.bookID, scope.userID, status, body,
		idempotencyTTL.Seconds())
	if err != nil {
		return err
	}
	return nil
}
