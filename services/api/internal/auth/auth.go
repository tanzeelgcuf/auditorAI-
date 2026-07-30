package auth

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/argon2"
)

type Service struct {
	db            *pgxpool.Pool
	jwtSecret     []byte
	accessTTL     time.Duration
	refreshTTL    time.Duration
}

type Claims struct {
	UserID string `json:"user_id"`
	FirmID string `json:"firm_id"`
	Role   string `json:"role"`
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

// HTTP Handlers
func (s *Service) HandleSignup(w http.ResponseWriter, r *http.Request) {
	// TODO: Implement firm + first admin user creation
	// Send email verification
	writeJSON(w, http.StatusCreated, map[string]string{"message": "Signup initiated, check email"})
}

func (s *Service) HandleLogin(w http.ResponseWriter, r *http.Request) {
	// TODO: Verify credentials, generate tokens
	writeJSON(w, http.StatusOK, TokenPair{
		AccessToken:  "access-token",
		RefreshToken: "refresh-token",
	})
}

func (s *Service) HandleLogout(w http.ResponseWriter, r *http.Request) {
	// TODO: Revoke refresh token
	writeJSON(w, http.StatusOK, map[string]string{"message": "Logged out"})
}

func (s *Service) HandleRefresh(w http.ResponseWriter, r *http.Request) {
	// TODO: Validate refresh token, issue new access token
	writeJSON(w, http.StatusOK, map[string]string{"access_token": "new-access-token"})
}

func (s *Service) HandleVerifyEmail(w http.ResponseWriter, r *http.Request) {
	// TODO: Verify token, mark user email_verified=true
	writeJSON(w, http.StatusOK, map[string]string{"message": "Email verified"})
}

func (s *Service) HandleForgotPassword(w http.ResponseWriter, r *http.Request) {
	// TODO: Generate reset token, send email
	writeJSON(w, http.StatusOK, map[string]string{"message": "If email exists, reset link sent"})
}

func (s *Service) HandleResetPassword(w http.ResponseWriter, r *http.Request) {
	// TODO: Validate token, update password
	writeJSON(w, http.StatusOK, map[string]string{"message": "Password reset"})
}

func (s *Service) HandleEnableTOTP(w http.ResponseWriter, r *http.Request) {
	// TODO: Generate TOTP secret, return QR code data
	writeJSON(w, http.StatusOK, map[string]string{"secret": "base32-secret", "qr_code": "data:image/png;base64,..."})
}

func (s *Service) HandleVerifyTOTP(w http.ResponseWriter, r *http.Request) {
	// TODO: Verify TOTP code, update user.totp_secret
	writeJSON(w, http.StatusOK, map[string]string{"message": "2FA enabled"})
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = v // json.NewEncoder(w).Encode(v) in real impl
}