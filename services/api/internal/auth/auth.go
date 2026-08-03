package auth

import (
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
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
	"golang.org/x/crypto/argon2"

	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/email"
)

type Service struct {
	db           *pgxpool.Pool
	jwtSecret    []byte
	accessTTL    time.Duration
	refreshTTL   time.Duration
	deniedTokens sync.Map
	emailSender  email.EmailSender
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

	return &Service{
		jwtSecret:  []byte(secret),
		accessTTL:  15 * time.Minute,
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

func (s *Service) isTokenDenied(jti string) bool {
	_, denied := s.deniedTokens.Load(jti)
	return denied
}

func (s *Service) denyToken(jti string) {
	s.deniedTokens.Store(jti, true)
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
}

func (s *Service) HandleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	var id, firmID, passwordHash, role string
	var emailVerified bool
	err := s.db.QueryRow(r.Context(),
		"SELECT id, firm_id, password_hash, role, email_verified FROM users WHERE email = $1",
		req.Email).Scan(&id, &firmID, &passwordHash, &role, &emailVerified)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid email or password"})
			return
		}
		slog.Error("login query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	if !VerifyPassword(req.Password, passwordHash) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid email or password"})
		return
	}

	if !emailVerified {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "email not verified"})
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
			s.denyToken(claims.ID)
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

	_, err = s.db.Exec(r.Context(),
		`UPDATE users SET password_hash = $1, password_reset_token = NULL, password_reset_expires = NULL WHERE id = $2`,
		hash, req.UserID)
	if err != nil {
		slog.Error("failed to update password", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"message": "Password reset"})
}

func (s *Service) HandleEnableTOTP(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value("user_id")
	if userID == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	var email string
	err := s.db.QueryRow(r.Context(),
		"SELECT email FROM users WHERE id = $1", userID).Scan(&email)
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

	writeJSON(w, http.StatusOK, map[string]string{
		"secret":  key.Secret(),
		"qr_code": key.URL(),
	})
}

type verifyTOTPRequest struct {
	Code   string `json:"code"`
	Secret string `json:"secret"`
}

func (s *Service) HandleVerifyTOTP(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value("user_id")
	if userID == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	var req verifyTOTPRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if req.Code == "" || req.Secret == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "code and secret are required"})
		return
	}

	valid := totp.Validate(req.Code, req.Secret)
	if !valid {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid TOTP code"})
		return
	}

	_, err := s.db.Exec(r.Context(),
		"UPDATE users SET totp_secret = $1 WHERE id = $2", req.Secret, userID)
	if err != nil {
		slog.Error("failed to store TOTP secret", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"message": "2FA enabled"})
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
