package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
	"golang.org/x/crypto/argon2"

	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/email"
)

type Service struct {
	db          *pgxpool.Pool
	jwtSecret   []byte
	accessTTL   time.Duration
	refreshTTL  time.Duration
	emailSender email.EmailSender
}

type Claims struct {
	UserID       string `json:"user_id"`
	FirmID       string `json:"firm_id"`
	Role         string `json:"role"`
	PortalBookID string `json:"portal_book_id,omitempty"`
	jwt.RegisteredClaims
}

type TokenPair struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

func NewService() *Service {
	secret := os.Getenv("JWT_SECRET")
	if secret == "" {
		slog.Warn("JWT_SECRET not set, using default (INSECURE)")
		secret = "dev-secret-change-in-production"
	}

	// DECIDED 2026-10-03 (prompt Part 1.3): the denylist covers the
	// refresh-token path only, and the access-token residual window after a
	// logout is bounded by THIS TTL. 15 minutes is the standard's tight end
	// (short-lived access tokens minimize the attack window); a logout kills
	// the session's future because the refresh is denied; adding a per-request
	// DB lookup in the auth middleware would trade a hot-path cost on every
	// request for a marginal gain on a window that is already short. So the
	// mitigation is the TTL itself, made env-configurable so an operator can
	// tighten it without a redeploy. An unset/invalid value keeps 15m.
	accessTTL := 15 * time.Minute
	if v := os.Getenv("ACCESS_TOKEN_TTL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 && d <= time.Hour {
			accessTTL = d
		} else {
			slog.Warn("ACCESS_TOKEN_TTL invalid or over 1h; keeping 15m", "value", v)
		}
	}

	return &Service{
		jwtSecret:  []byte(secret),
		accessTTL:  accessTTL,
		refreshTTL: 7 * 24 * time.Hour,
	}
}

func (s *Service) SetDB(db *pgxpool.Pool) {
	s.db = db
}

func (s *Service) SetEmailSender(sender email.EmailSender) {
	s.emailSender = sender
}

// HashPassword uses Argon2id
func HashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}

	hash := argon2.IDKey([]byte(password), salt, 1, 64*1024, 4, 32)
	return fmt.Sprintf("$argon2id$v=19$m=65536,t=1,p=4$%s$%s",
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(hash)), nil
}

// VerifyPassword checks Argon2id hash
func VerifyPassword(password, encodedHash string) bool {
	parts := strings.Split(encodedHash, "$")
	if len(parts) != 6 {
		return false
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}

	hash, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}

	testHash := argon2.IDKey([]byte(password), salt, 1, 64*1024, 4, uint32(len(hash)))
	return subtle.ConstantTimeCompare(hash, testHash) == 1
}

func (s *Service) GenerateTokens(userID, firmID, role string) (*TokenPair, error) {
	now := time.Now()
	accessClaims := Claims{
		UserID: userID,
		FirmID: firmID,
		Role:   role,
		RegisteredClaims: jwt.RegisteredClaims{
			// ID is the jti, and it is REQUIRED: without it every token shares
			// the empty jti and HandleLogout's denyToken("") denied EVERY
			// refresh in the deployment after one logout (read from the code
			// 2026-09-28, the denylist build's first finding — the 09-05
			// status doc's framing had it backwards).
			ID:        uuid.NewString(),
			ExpiresAt: jwt.NewNumericDate(now.Add(s.accessTTL)),
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			Subject:   userID,
		},
	}

	refreshClaims := Claims{
		UserID: userID,
		FirmID: firmID,
		Role:   role,
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        uuid.NewString(), // the jti — see accessClaims above
			ExpiresAt: jwt.NewNumericDate(now.Add(s.refreshTTL)),
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			Subject:   userID,
		},
	}

	accessToken := jwt.NewWithClaims(jwt.SigningMethodHS256, accessClaims)
	refreshToken := jwt.NewWithClaims(jwt.SigningMethodHS256, refreshClaims)

	accessStr, err := accessToken.SignedString(s.jwtSecret)
	if err != nil {
		return nil, err
	}

	refreshStr, err := refreshToken.SignedString(s.jwtSecret)
	if err != nil {
		return nil, err
	}

	return &TokenPair{
		AccessToken:  accessStr,
		RefreshToken: refreshStr,
	}, nil
}

// GeneratePortalTokens issues a token pair for a client-portal user scoped to a
// single book (Role="portal_user", PortalBookID set). Read-only portal access only.
func (s *Service) GeneratePortalTokens(portalUserID, bookID string) (*TokenPair, error) {
	now := time.Now()
	accessClaims := Claims{
		UserID:       portalUserID,
		Role:         "portal_user",
		PortalBookID: bookID,
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        uuid.NewString(), // the jti — required, see GenerateTokens
			ExpiresAt: jwt.NewNumericDate(now.Add(s.accessTTL)),
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			Subject:   portalUserID,
		},
	}
	refreshClaims := Claims{
		UserID:       portalUserID,
		Role:         "portal_user",
		PortalBookID: bookID,
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        uuid.NewString(), // the jti — required, see GenerateTokens
			ExpiresAt: jwt.NewNumericDate(now.Add(s.refreshTTL)),
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			Subject:   portalUserID,
		},
	}
	accessToken := jwt.NewWithClaims(jwt.SigningMethodHS256, accessClaims)
	refreshToken := jwt.NewWithClaims(jwt.SigningMethodHS256, refreshClaims)
	accessStr, err := accessToken.SignedString(s.jwtSecret)
	if err != nil {
		return nil, err
	}
	refreshStr, err := refreshToken.SignedString(s.jwtSecret)
	if err != nil {
		return nil, err
	}
	return &TokenPair{AccessToken: accessStr, RefreshToken: refreshStr}, nil
}

func (s *Service) ValidateAccessToken(tokenStr string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(tokenStr, &Claims{}, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method")
		}
		return s.jwtSecret, nil
	})

	if err != nil {
		return nil, err
	}

	if claims, ok := token.Claims.(*Claims); ok && token.Valid {
		return claims, nil
	}

	return nil, errors.New("invalid token")
}

// isTokenDenied consults the DB-backed denylist. Until 2026-09-28 this was an
// in-memory sync.Map, and no token was EVER issued with a jti claim — so
// HandleLogout's denyToken(claims.ID) denied the EMPTY string and this check
// returned true for every refresh token in the deployment: one logout,
// anywhere, denied every refresh until the process restarted. Tokens now
// carry a unique jti at every issuance site, and the list is the
// denied_tokens table (s.db IS the sys pool for this service — main.go:155 —
// correct for a public pre-auth route, cross-firm by design).
//
// An empty jti returns false, deliberately: it means "issued without a jti
// claim" (pre-fix tokens, or a forged token — which already failed the
// signature check before this lookup), and denying them all would be the
// lockout bug. A check that cannot run fails CLOSED (the project's
// asymmetry): a DB error denies, and the outage is visible.
func (s *Service) isTokenDenied(jti string) bool {
	if jti == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var denied bool
	err := s.db.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM denied_tokens WHERE jti = $1 AND expires_at > now())`,
		jti).Scan(&denied)
	if err != nil {
		slog.Error("failed to check token denial — failing closed", "error", err)
		return true
	}
	return denied
}

// denyToken persists a refresh token's revocation. The in-memory sync.Map
// this replaces was never revoked on another replica and died with the
// process. expires_at is TTL-aligned: it carries the token's own exp, so the
// row self-destructs in meaning exactly when the token would have expired
// anyway, and the lazy cleanup on every INSERT keeps the table holding only
// still-valid denials. The refresh's own signature check rejects an expired
// token before this list is consulted, so a stale row is harmless, not a
// lockout. context.Background(), not r.Context(): the denylist write must
// outlive a client who hangs up mid-logout (rule 12) — a lost denial leaves a
// long-lived token the user believes is revoked.
//
// The INSERT's error is RETURNED, not logged: the denial is the only record
// of the revocation, so a swallowed failure would leave that long-lived token
// valid with nothing anywhere saying so — the caller's 500 and retry is the
// visible failure. The lazy cleanup's error IS logged and continue: it is
// hygiene (expired rows), and allowlisted in
// scripts/check_cancellable_audit_writes.py for exactly that reason.
func (s *Service) denyToken(jti string, expiresAt time.Time, userID string) error {
	if jti == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s.db.Exec(ctx,
		`INSERT INTO denied_tokens (jti, user_id, denied_at, expires_at)
		 VALUES ($1, $2, now(), $3)
		 ON CONFLICT (jti) DO UPDATE SET denied_at = now()
		 WHERE denied_tokens.expires_at < now()`,
		jti, userID, expiresAt); err != nil {
		return err
	}
	if _, err := s.db.Exec(ctx,
		`DELETE FROM denied_tokens WHERE expires_at < now()`); err != nil {
		slog.Warn("denylist cleanup failed", "error", err)
	}
	return nil
}

// consumeRecoveryCode atomically spends one recovery code inside the login's
// transaction. Single-use is enforced by the UPDATE's `AND used = false` in
// the same statement that marks it used — two simultaneous logins cannot both
// consume the same code — and the transaction holds the users row lock, so a
// login that fails later rolls the consumption back and the code survives.
// Returns false when no matching unused code exists (a wrong code, or one
// already spent), which the caller treats as a failed second factor.
func (s *Service) consumeRecoveryCode(ctx context.Context, tx pgx.Tx, userID string, submitted string) (bool, error) {
	hash := HashRecoveryCode(submitted)
	tag, err := tx.Exec(ctx,
		`UPDATE recovery_codes SET used = true, used_at = now()
		  WHERE user_id = $1 AND code_hash = $2 AND used = false`,
		userID, hash)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// HandleGenerateRecoveryCodes issues a fresh batch of recovery codes for the
// authenticated user. Enrollment in 2FA is required first — the codes are the
// alternative to a TOTP code, meaningless without the factor. The old batch
// is deleted in the same transaction that stores the new one (regenerating
// invalidates it immediately), and the plaintext is returned ONCE — the
// database holds SHA-256 hashes only. Must be mounted behind
// middleware.Authenticator, like the other /v1/totp routes.
func (s *Service) HandleGenerateRecoveryCodes(w http.ResponseWriter, r *http.Request) {	userID := UserIDFrom(r.Context())
	if userID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	var alreadyEnabled bool
	if err := s.db.QueryRow(r.Context(),
		`SELECT totp_secret IS NOT NULL FROM users WHERE id = $1`, userID).
		Scan(&alreadyEnabled); err != nil {
		slog.Error("recovery: enrollment check failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if !alreadyEnabled {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "enroll in two-factor authentication before generating recovery codes",
		})
		return
	}

	codes, err := GenerateRecoveryCodes(recoveryCodeCount)
	if err != nil {
		slog.Error("recovery: generation failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	tx, err := s.db.Begin(r.Context())
	if err != nil {
		slog.Error("recovery: begin failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if _, err := tx.Exec(r.Context(),
		`DELETE FROM recovery_codes WHERE user_id = $1`, userID); err != nil {
		_ = tx.Rollback(r.Context())
		slog.Error("recovery: old batch delete failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	for _, code := range codes {
		if _, err := tx.Exec(r.Context(),
			`INSERT INTO recovery_codes (user_id, code_hash) VALUES ($1, $2)`,
			userID, HashRecoveryCode(code)); err != nil {
			_ = tx.Rollback(r.Context())
			slog.Error("recovery: code insert failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
	}
	if err := tx.Commit(r.Context()); err != nil {
		slog.Error("recovery: commit failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"codes": codes})
}

// signupRequest is the JSON body for HandleSignup
type signupRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	FirmName string `json:"firm_name"`
}

// HTTP Handlers
func (s *Service) HandleSignup(w http.ResponseWriter, r *http.Request) {
	var req signupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if req.Email == "" || req.Password == "" || req.FirmName == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "email, password, and firm_name are required"})
		return
	}

	hash, err := HashPassword(req.Password)
	if err != nil {
		slog.Error("failed to hash password", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	verificationToken := make([]byte, 32)
	if _, err := rand.Read(verificationToken); err != nil {
		slog.Error("failed to generate verification token", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	tokenHex := hex.EncodeToString(verificationToken)
	expiresAt := time.Now().Add(48 * time.Hour)

	conn, err := s.db.Acquire(r.Context())
	if err != nil {
		slog.Error("failed to acquire connection", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	defer conn.Release()

	tx, err := conn.Begin(r.Context())
	if err != nil {
		slog.Error("failed to begin transaction", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	defer tx.Rollback(r.Context())

	var firmID string
	err = tx.QueryRow(r.Context(),
		"INSERT INTO firms (name) VALUES ($1) RETURNING id", req.FirmName).Scan(&firmID)
	if err != nil {
		slog.Error("failed to create firm", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create firm"})
		return
	}

	var userID string
	err = tx.QueryRow(r.Context(),
		`INSERT INTO users (firm_id, email, password_hash, role, email_verification_token, email_verification_expires)
		 VALUES ($1, $2, $3, 'firm_admin', $4, $5) RETURNING id`,
		firmID, req.Email, hash, tokenHex, expiresAt).Scan(&userID)
	if err != nil {
		slog.Error("failed to create user", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create user"})
		return
	}

	if err := tx.Commit(r.Context()); err != nil {
		slog.Error("failed to commit transaction", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	// Send verification email (async fire-and-forget is fine; the user will see
	// the same message regardless, and the token is already stored).
	if s.emailSender != nil {
		verifyURL := fmt.Sprintf("%s/verify-email?token=%s&user_id=%s", os.Getenv("APP_BASE_URL"), tokenHex, userID)
		if verifyURL == "/verify-email?token="+tokenHex+"&user_id="+userID {
			verifyURL = fmt.Sprintf("https://auditor.app/verify-email?token=%s&user_id=%s", tokenHex, userID)
		}
		html, _ := email.Render(email.VerifyEmailTemplate, email.TemplateData{
			VerifyURL:       verifyURL,
			FirmName:        req.FirmName,
			UserName:        req.Email,
			ExpirationHours: 48,
		})
		if err := s.emailSender.Send(r.Context(), req.Email, "Verify your AI Auditor account", html); err != nil {
			slog.Error("failed to send verification email", "error", err, "email", req.Email)
		}
	}

	slog.Info("user signed up", "user_id", userID, "firm_id", firmID, "email", req.Email)
	writeJSON(w, http.StatusCreated, map[string]string{"message": "Signup initiated, check email"})
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	// TOTPCode is the second factor. Both shipped clients already send this
	// field (apps/web login page, apps/mobile LoginScreen); until 2026-09-04 the
	// server had no field to decode it into, so it was silently discarded and a
	// user with 2FA "enabled" could log in with a password alone.
	TOTPCode string `json:"totp_code"`
	// RecoveryCode is the ALTERNATIVE second factor, submitted in place of a
	// TOTP code by a user who lost their authenticator. When present it
	// replaces the TOTP check entirely; see consumeRecoveryCode.
	RecoveryCode string `json:"recovery_code"`
}

// invalidCredentials is the ONE response every rejected login gets: unknown
// email, wrong password, and locked account are indistinguishable. Defined once
// so the three call sites cannot drift apart, which is how a lockout turns into a
// user-enumeration oracle.
func invalidCredentials(w http.ResponseWriter) {
	writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid email or password"})
}

// HandleLogin authenticates a user and issues tokens.
//
// IT RUNS IN ONE TRANSACTION WITH `SELECT ... FOR UPDATE` on the user row. That
// is not incidental: the per-account failed-attempt counter (see lockout.go) is a
// read-check-increment, and without the row lock two concurrent attempts both read
// the same count and both write count+1, so the ceiling could be raised by
// parallelism alone. The lock serialises attempts against ONE account, which is
// exactly the contention this control wants to create. It costs nothing on the
// happy path — Argon2id at 64MB already dominates the latency of this handler.
func (s *Service) HandleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	conn, err := s.db.Acquire(r.Context())
	if err != nil {
		slog.Error("login: failed to acquire connection", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	defer conn.Release()

	tx, err := conn.Begin(r.Context())
	if err != nil {
		slog.Error("login: failed to begin transaction", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	defer tx.Rollback(r.Context())

	var id, firmID, passwordHash, role string
	var emailVerified bool
	var totpSecret, totpLastCode *string
	var totpLastUsedAt *time.Time
	var failedAttempts int
	var totpGraceUsed bool
	var lockedUntil, lastFailedAt *time.Time
	err = tx.QueryRow(r.Context(),
		`SELECT id, firm_id, password_hash, role, email_verified,
		        totp_secret, totp_last_code, totp_last_used_at,
		        failed_login_attempts, locked_until, last_failed_login_at,
		        totp_grace_used
		   FROM users WHERE email = $1
		   FOR UPDATE`,
		req.Email).Scan(&id, &firmID, &passwordHash, &role, &emailVerified,
		&totpSecret, &totpLastCode, &totpLastUsedAt,
		&failedAttempts, &lockedUntil, &lastFailedAt, &totpGraceUsed)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			invalidCredentials(w)
			return
		}
		slog.Error("login query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	now := time.Now().UTC()
	lock := LockoutState{FailedAttempts: failedAttempts}
	if lockedUntil != nil {
		lock.LockedUntil = *lockedUntil
	}
	if lastFailedAt != nil {
		lock.LastFailedAt = *lastFailedAt
	}

	// LOCKED: return before the password is even looked at.
	//
	// Verifying the password here and reporting the lock separately would be
	// friendlier to the real user and fatal to the control: an attacker would hammer
	// through the lock window watching for the response that differs, and get an
	// unlimited password-correctness oracle that never touches the counter. So the
	// cost is accepted — a locked-out user sees "invalid email or password" until
	// the window expires. See lockout.go, IsLocked contract rule 1.
	if IsLocked(lock, now) {
		slog.Warn("login attempt on locked account", "user_id", id,
			"attempts", lock.FailedAttempts, "locked_until", lock.LockedUntil)
		invalidCredentials(w)
		return
	}

	if !VerifyPassword(req.Password, passwordHash) {
		s.persistLoginFailure(r.Context(), tx, id, lock, now, "bad_password")
		invalidCredentials(w)
		return
	}

	if !emailVerified {
		// Neither a guess nor a success: the credentials were right, so nothing is
		// counted, and the counter is NOT cleared either — a 403 path that zeroes it
		// would be a free reset for anyone holding the password.
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "email not verified"})
		return
	}

	// Second factor. Ordered after the password on purpose: a caller who cannot
	// present the password learns nothing about whether the account has 2FA.
	sf := SecondFactorState{}
	if totpSecret != nil {
		sf.Secret = *totpSecret
	}
	if totpLastCode != nil {
		sf.LastCode = *totpLastCode
	}
	if totpLastUsedAt != nil {
		sf.LastUsedAt = *totpLastUsedAt
	}

	// TOTP mandatory for firm_admin (added 2026-10-04): a firm_admin with no
	// enrolled factor is gated BEFORE tokens issue, and the gate is a hard
	// one — not a dismissible prompt. Rule-11-grade ordering: the grace login
	// is consumed ONLY in the same transaction that issues tokens (the burn
	// UPDATE below), so a login that fails later never spends the grace, and
	// an attacker who cannot present the password learns nothing about it.
	// The grace flag is false by default, so a NEW firm_admin account also
	// gets one grace login — the gate's other half: after the grace is spent
	// and the factor is still absent, the login is refused with
	// enrollment_required, which the client turns into a redirect to the
	// enrollment flow.
	requireEnrollment := false
	if role == "firm_admin" && sf.Secret == "" {
		if !totpGraceUsed {
			requireEnrollment = true // the grace login: allowed; consumed at token issuance
		} else {
			slog.Warn("login blocked: firm_admin without 2FA after grace", "user_id", id)
			// A problem-shaped response: the client's error wrapper parses the
			// detail, so the gate's purpose ("complete enrollment") travels in
			// the message the user sees, and the type names the condition for
			// a programmatic redirect.
			writeProblem(w, http.StatusForbidden, "https://ai-auditor.dev/errors/enrollment-required",
				"two-factor authentication is required for your role — complete enrollment to continue")
			return
		}
	}

	// A recovery code, when submitted, REPLACES the TOTP check: it is the
	// account's alternative second factor (the research's model, applied —
	// GitHub's: a backup code works in place of the factor). Validated inside
	// THIS transaction: the consumption is atomic with the login (the tx holds
	// the users row lock), so a failure before the commit leaves the code
	// unconsumed, and two simultaneous logins cannot both spend the same code.
	// A WRONG code is a guess and counts — the same lockout counter as a bad
	// password or a bad TOTP code, so switching factors buys an attacker
	// nothing.
	if req.RecoveryCode != "" {
		consumed, rcErr := s.consumeRecoveryCode(r.Context(), tx, id, req.RecoveryCode)
		if rcErr != nil {
			slog.Error("recovery code check failed", "error", rcErr, "user_id", id)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		if !consumed {
			s.persistLoginFailure(r.Context(), tx, id, lock, now, "bad_recovery_code")
			slog.Warn("login blocked by second factor", "user_id", id, "reason", "invalid recovery code")
			writeJSON(w, http.StatusUnauthorized, map[string]interface{}{
				"error":         "invalid recovery code",
				"totp_required": true,
			})
			return
		}
	} else if err := CheckSecondFactor(sf, req.TOTPCode, now); err != nil {
		// A WRONG or REPLAYED code is a guess and counts. A MISSING code does not:
		// no candidate secret was tested, and both shipped clients render the code
		// as one optional field on the same form as the password, so an enrolled
		// user submitting the form empty is ordinary user error. Counting it would
		// lock real users out for something that reveals nothing.
		if !errors.Is(err, ErrTOTPRequired) {
			s.persistLoginFailure(r.Context(), tx, id, lock, now, "bad_totp")
		}
		slog.Warn("login blocked by second factor", "user_id", id, "reason", err.Error())
		writeJSON(w, http.StatusUnauthorized, map[string]interface{}{
			"error":         err.Error(),
			"totp_required": true,
		})
		return
	}

	// Fully authenticated. ONE statement clears the lockout and burns the TOTP code,
	// because both must be true of the same commit:
	//   - the counter must not clear before the second factor has passed (every TOTP
	//     guess carries the correct password, so an earlier reset would hold the
	//     counter at 1 forever and the six-digit ceiling would not exist);
	//   - the code must not stay spendable after tokens are issued.
	// A failure here fails the login rather than issuing tokens on half-written
	// state.
	burn := ""
	if sf.Secret != "" {
		burn = NormalizeTOTPCode(req.TOTPCode)
	}
	// The grace login's consumption lives HERE, in the same transaction that
	// issues tokens — the rule-11 ordering: a login that fails before this
	// commit never spends the grace, and the counter-clear + the grace
	// consumption + the token issuance are all true of the same commit.
	graceApplies := requireEnrollment
	if _, err := tx.Exec(r.Context(),
		`UPDATE users
		    SET failed_login_attempts = 0,
		        locked_until          = NULL,
		        last_failed_login_at  = NULL,
		        totp_last_code    = CASE WHEN $1 = '' THEN totp_last_code    ELSE $1    END,
		        totp_last_used_at = CASE WHEN $1 = '' THEN totp_last_used_at ELSE now() END,
		        totp_grace_used   = CASE WHEN $3 THEN true ELSE totp_grace_used END
		  WHERE id = $2`,
		burn, id, graceApplies); err != nil {
		slog.Error("failed to record successful login", "error", err, "user_id", id)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		slog.Error("failed to commit successful login", "error", err, "user_id", id)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	tokens, err := s.GenerateTokens(id, firmID, role)
	if err != nil {
		slog.Error("failed to generate tokens", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	writeJSON(w, http.StatusOK, tokens)
}

// persistLoginFailure records ONE failed attempt and COMMITS it.
//
// The commit is the whole point. The handler returns 401 immediately afterwards,
// and the deferred Rollback would otherwise discard the increment — a
// failed-attempt counter that rolls back is a counter that does not exist.
//
// IT ALSO SURVIVES THE CLIENT HANGING UP. `r.Context()` is cancelled the instant
// the client's socket closes, and both the Exec and the Commit below would then
// return context.Canceled and drop the increment. That is the one write in this
// handler whose loss is fail-OPEN, so cancellation is stripped here and replaced
// with a deadline of its own. The pooled database connection has nothing to do
// with the client's socket, so it is still perfectly usable. HandleLogin's
// SUCCESS commit deliberately keeps the request context: if that one is
// cancelled, no tokens are issued, the code is not burned and the counter is not
// cleared, which is fail-CLOSED and correct. The asymmetry is the point.
//
// A write error is logged and the caller still returns 401: the request is denied
// either way, and turning a database problem into a 500 here would hand an
// attacker a way to make logins fail loudly. The Error log is the signal that the
// ceiling is not being recorded.
func (s *Service) persistLoginFailure(ctx context.Context, tx pgx.Tx, userID string, st LockoutState, now time.Time, reason string) {
	if IsLocked(st, now) {
		// Unreachable from HandleLogin, which returns before it gets here. Kept
		// because the invariant belongs with the write: a failure inside the window
		// must not extend it (lockout.go, IsLocked contract rule 2).
		return
	}
	next := LockoutAfterFailure(st, now)

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	var until *time.Time
	if !next.LockedUntil.IsZero() {
		until = &next.LockedUntil
	}
	if _, err := tx.Exec(ctx,
		`UPDATE users
		    SET failed_login_attempts = $1,
		        locked_until          = $2,
		        last_failed_login_at  = $3
		  WHERE id = $4`,
		next.FailedAttempts, until, next.LastFailedAt, userID); err != nil {
		slog.Error("failed to record login failure", "error", err, "user_id", userID, "reason", reason)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		slog.Error("failed to commit login failure", "error", err, "user_id", userID, "reason", reason)
		return
	}

	if until != nil {
		slog.Warn("account locked after repeated failed logins", "user_id", userID,
			"reason", reason, "attempts", next.FailedAttempts, "locked_until", *until)
		return
	}
	slog.Info("failed login recorded", "user_id", userID, "reason", reason,
		"attempts", next.FailedAttempts, "threshold", LockoutThreshold)
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

func (s *Service) HandleLogout(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	token, _, err := new(jwt.Parser).ParseUnverified(req.RefreshToken, &Claims{})
	if err == nil {
		if claims, ok := token.Claims.(*Claims); ok {
			exp := time.Now().Add(24 * time.Hour)
			if claims.ExpiresAt != nil {
				exp = claims.ExpiresAt.Time
			}
			// The denial write's failure fails the request: the write IS the
			// logout's only work, and returning "Logged out" on a failed
			// revocation would leave the token valid while the user believes
			// otherwise. The retry is idempotent (ON CONFLICT guards the
			// re-INSERT).
			if denyErr := s.denyToken(claims.ID, exp, claims.UserID); denyErr != nil {
				slog.Error("logout: failed to persist token denial", "error", denyErr)
				writeJSON(w, http.StatusInternalServerError, map[string]string{
					"error": "logout failed: the session could not be revoked — retry",
				})
				return
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{"message": "Logged out"})
}

func (s *Service) HandleRefresh(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	if req.RefreshToken == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "refresh_token is required"})
		return
	}

	claims, err := jwt.ParseWithClaims(req.RefreshToken, &Claims{}, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method")
		}
		return s.jwtSecret, nil
	})
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid or expired refresh token"})
		return
	}

	c := claims.Claims.(*Claims)
	if s.isTokenDenied(c.ID) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "refresh token has been revoked"})
		return
	}

	accessStr, err := jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{
		UserID: c.UserID,
		FirmID: c.FirmID,
		Role:   c.Role,
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        uuid.NewString(), // the jti — required, see GenerateTokens
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(s.accessTTL)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			NotBefore: jwt.NewNumericDate(time.Now()),
			Subject:   c.UserID,
		},
	}).SignedString(s.jwtSecret)
	if err != nil {
		slog.Error("failed to sign new access token", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"access_token": accessStr})
}

func (s *Service) HandleVerifyEmail(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	userID := r.URL.Query().Get("user_id")
	if token == "" || userID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "token and user_id are required"})
		return
	}

	result, err := s.db.Exec(r.Context(),
		`UPDATE users SET email_verified = true,
			email_verification_token = NULL,
			email_verification_expires = NULL
		 WHERE id = $1 AND email_verification_token = $2
		   AND (email_verification_expires IS NULL OR email_verification_expires > now())`,
		userID, token)
	if err != nil {
		slog.Error("failed to verify email", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if result.RowsAffected() == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "invalid or expired verification token"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"message": "Email verified"})
}

type forgotPasswordRequest struct {
	Email string `json:"email"`
}

func (s *Service) HandleForgotPassword(w http.ResponseWriter, r *http.Request) {
	var req forgotPasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	if req.Email == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "email is required"})
		return
	}

	resetToken := make([]byte, 32)
	if _, err := rand.Read(resetToken); err != nil {
		slog.Error("failed to generate reset token", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	tokenHex := hex.EncodeToString(resetToken)
	expiresAt := time.Now().Add(1 * time.Hour)

	_, err := s.db.Exec(r.Context(),
		`UPDATE users SET password_reset_token = $1, password_reset_expires = $2 WHERE email = $3`,
		tokenHex, expiresAt, req.Email)
	if err != nil {
		slog.Error("failed to store reset token", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	// Send reset email (fire-and-forget; token already stored).
	if s.emailSender != nil {
		resetURL := fmt.Sprintf("%s/reset-password?token=%s&user_id=%s", os.Getenv("APP_BASE_URL"), tokenHex, req.Email)
		if resetURL == "/reset-password?token="+tokenHex+"&user_id="+req.Email {
			resetURL = fmt.Sprintf("https://auditor.app/reset-password?token=%s&user_id=%s", tokenHex, req.Email)
		}
		html, _ := email.Render(email.ResetPasswordTemplate, email.TemplateData{
			ResetURL:        resetURL,
			FirmName:        "AI Auditor",
			UserName:        req.Email,
			ExpirationHours: 1,
		})
		if err := s.emailSender.Send(r.Context(), req.Email, "Reset your AI Auditor password", html); err != nil {
			slog.Error("failed to send reset email", "error", err, "email", req.Email)
		}
	}

	// Always return same message regardless of whether email exists (anti-enumeration)
	writeJSON(w, http.StatusOK, map[string]string{"message": "If email exists, reset link sent"})
}

type resetPasswordRequest struct {
	UserID   string `json:"user_id"`
	Token    string `json:"token"`
	Password string `json:"password"`
}

func (s *Service) HandleResetPassword(w http.ResponseWriter, r *http.Request) {
	var req resetPasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if req.UserID == "" || req.Token == "" || req.Password == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "user_id, token, and password are required"})
		return
	}

	// Validate token
	var storedToken string
	var expiresAt time.Time
	err := s.db.QueryRow(r.Context(),
		"SELECT password_reset_token, password_reset_expires FROM users WHERE id = $1",
		req.UserID).Scan(&storedToken, &expiresAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "invalid or expired reset token"})
			return
		}
		slog.Error("failed to query reset token", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	if storedToken == "" || storedToken != req.Token || time.Now().After(expiresAt) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid or expired reset token"})
		return
	}

	hash, err := HashPassword(req.Password)
	if err != nil {
		slog.Error("failed to hash new password", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	// The lockout is cleared here as well. Without this, a user who got locked out,
	// concluded they had forgotten their password, and reset it would still be
	// refused with "invalid email or password" until the window expired — with a
	// password they now know is correct. Safe to clear: reaching this line requires
	// the single-use token that was mailed to the address on the account, which is a
	// stronger proof than the counter is protecting.
	_, err = s.db.Exec(r.Context(),
		`UPDATE users
		    SET password_hash          = $1,
		        password_reset_token   = NULL,
		        password_reset_expires = NULL,
		        failed_login_attempts  = 0,
		        locked_until           = NULL,
		        last_failed_login_at   = NULL
		  WHERE id = $2`,
		hash, req.UserID)
	if err != nil {
		slog.Error("failed to update password", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"message": "Password reset"})
}

// HandleEnableTOTP begins enrollment: it generates a secret, persists it as
// PENDING, and returns it once so the caller can load it into an authenticator.
// The secret does not become the account's second factor until
// HandleVerifyTOTP proves the device can compute codes from it.
//
// Must be mounted behind middleware.Authenticator — it reads the caller's
// identity from the request context and will refuse the request without it.
func (s *Service) HandleEnableTOTP(w http.ResponseWriter, r *http.Request) {
	userID := UserIDFrom(r.Context())
	if userID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	var email string
	var alreadyEnabled bool
	err := s.db.QueryRow(r.Context(),
		"SELECT email, totp_secret IS NOT NULL FROM users WHERE id = $1", userID).
		Scan(&email, &alreadyEnabled)
	if err != nil {
		slog.Error("failed to get user email for TOTP", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      "AI Auditor",
		AccountName: email,
		Period:      30,
		SecretSize:  20,
		Digits:      otp.DigitsSix,
		Algorithm:   otp.AlgorithmSHA1,
	})
	if err != nil {
		slog.Error("failed to generate TOTP key", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to generate TOTP secret"})
		return
	}

	// Persist as pending. Any earlier unfinished enrollment is overwritten, which
	// is correct: only one device can be mid-enrollment at a time, and the live
	// totp_secret is untouched until verify succeeds — so a re-enroll that is
	// abandoned halfway cannot lock the user out of an already-working factor.
	if _, err := s.db.Exec(r.Context(),
		"UPDATE users SET totp_pending_secret = $1 WHERE id = $2", key.Secret(), userID); err != nil {
		slog.Error("failed to store pending TOTP secret", "error", err, "user_id", userID)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	slog.Info("TOTP enrollment started", "user_id", userID, "replacing_existing", alreadyEnabled)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"secret":           key.Secret(),
		"qr_code":          key.URL(),
		"already_enabled":  alreadyEnabled,
		"confirm_endpoint": "/v1/totp/verify",
	})
}

type verifyTOTPRequest struct {
	Code string `json:"code"`
}

// HandleVerifyTOTP completes enrollment by validating a code against the secret
// this server generated and stored, then promoting it to the live factor.
//
// It deliberately takes NO secret from the request body. Until 2026-09-04 it did:
// it validated `code` against `secret` from the same JSON object, so the check
// proved only that the caller could run a TOTP library — an attacker (or an
// honest client with a bug) could generate a keypair locally, send a matching
// pair, and have the server store a secret the real user's authenticator had
// never seen. The stored secret is now the only one considered.
func (s *Service) HandleVerifyTOTP(w http.ResponseWriter, r *http.Request) {
	userID := UserIDFrom(r.Context())
	if userID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	var req verifyTOTPRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	code := NormalizeTOTPCode(req.Code)
	if len(code) != 6 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "a 6-digit code is required"})
		return
	}

	var pending *string
	err := s.db.QueryRow(r.Context(),
		"SELECT totp_pending_secret FROM users WHERE id = $1", userID).Scan(&pending)
	if err != nil {
		slog.Error("failed to read pending TOTP secret", "error", err, "user_id", userID)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if pending == nil || *pending == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "no pending enrollment; call /v1/totp/enable first",
		})
		return
	}

	if !totp.Validate(code, *pending) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid TOTP code"})
		return
	}

	// Promote pending to live in one statement, and seed the replay memory with
	// the code just used so it cannot immediately be replayed at login. The
	// `totp_pending_secret IS NOT NULL` guard makes this a no-op if a concurrent
	// request already consumed the same enrollment.
	tag, err := s.db.Exec(r.Context(),
		`UPDATE users
		    SET totp_secret = totp_pending_secret,
		        totp_pending_secret = NULL,
		        totp_enabled_at = now(),
		        totp_last_code = $1,
		        totp_last_used_at = now()
		  WHERE id = $2 AND totp_pending_secret IS NOT NULL`,
		code, userID)
	if err != nil {
		slog.Error("failed to store TOTP secret", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if tag.RowsAffected() == 0 {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "enrollment already completed"})
		return
	}

	slog.Info("TOTP enabled", "user_id", userID)
	writeJSON(w, http.StatusOK, map[string]string{
		"message": "2FA enabled",
		"note":    "Generate recovery codes at POST /v1/totp/recovery — the plaintext is shown once; store them offline.",
	})
}

// HandleTOTPStatus reports whether the authenticated user has 2FA enrolled.
// Behind the authenticator; the sysPool read scoped to the JWT's own user —
// the settings page's status display, which the enrollment UI needs before
// deciding whether to show the enroll flow or the current state.
func (s *Service) HandleTOTPStatus(w http.ResponseWriter, r *http.Request) {
	userID := UserIDFrom(r.Context())
	if userID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	var enabled bool
	if err := s.db.QueryRow(r.Context(),
		`SELECT totp_secret IS NOT NULL FROM users WHERE id = $1`, userID).Scan(&enabled); err != nil {
		slog.Error("totp status check failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"enabled": enabled})
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
