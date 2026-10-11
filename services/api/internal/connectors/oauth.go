// OAuth2 authorization-code flow for the connectors. The callback route is
// PUBLIC — the provider redirects the user's browser here and no JWT exists —
// so the `state` parameter IS the callback's authentication. It is
// HMAC-signed and stateless (no table): user|book|provider|expiry, signed
// with the JWT secret (the same trust domain — one fewer secret to manage),
// verified with a constant-time compare and an expiry check before anything
// in it is trusted. The pattern mirrors the Stripe webhook's: the route is
// public because the caller cannot present a JWT, and the handler
// authenticates its own caller.

package connectors

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// oauthStateTTL bounds how long an authorize link is valid. The user clicks
// through to the provider and back; 10 minutes covers that plus reading time.
const oauthStateTTL = 10 * time.Minute

// signOAuthState returns the signed state:
// "user|book|expiry|<mac>". The user/book/expiry travel IN THE CLEAR on
// purpose — the callback needs the book to store the connection, and an
// opaque MAC's inputs cannot be recovered from the MAC. The MAC is over
// user|book|provider|expiry, so it is the INTEGRITY proof, not the secrecy
// one: a tampered user/book/expiry refuses at the callback.
func (s *Service) signOAuthState(user, book, provider string, expiry time.Time) string {
	msg := user + "|" + book + "|" + provider + "|" + strconv.FormatInt(expiry.Unix(), 10)
	mac := hmac.New(sha256.New, s.stateSecret)
	mac.Write([]byte(msg))
	return user + "|" + book + "|" + strconv.FormatInt(expiry.Unix(), 10) + "|" + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// verifyOAuthState checks the state's four parts against a re-computed MAC
// and the expiry. Any mismatch refuses.
func (s *Service) verifyOAuthState(state, provider string) (string, string, bool) {
	parts := strings.Split(state, "|")
	if len(parts) != 4 {
		return "", "", false
	}
	user, book, expStr, macB64 := parts[0], parts[1], parts[2], parts[3]
	expiry, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil {
		return "", "", false
	}
	if time.Now().Unix() > expiry {
		return "", "", false
	}
	msg := user + "|" + book + "|" + provider + "|" + expStr
	mac := hmac.New(sha256.New, s.stateSecret)
	mac.Write([]byte(msg))
	expected := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(macB64)) {
		return "", "", false
	}
	return user, book, true
}

// tokenResponse is the RFC 6749 token endpoint's response. Both providers
// return this shape; QBO's expires_in is seconds, Xero's is too.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	TokenType    string `json:"token_type"`
}

// exchangeCode trades the OAuth authorization code for a token pair. One
// implementation serves both providers: the shape is the same RFC 6749
// request (client credentials + code + redirect_uri) against each provider's
// token URL.
func (s *Service) exchangeCode(ctx context.Context, cfg ProviderConfig, clientID, clientSecret, code, redirectURI string) (*tokenResponse, error) {
	return s.tokenRequest(ctx, cfg.TokenURL, clientID, clientSecret, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
	})
}

// refreshAccessToken trades a refresh token for a new pair. QBO expires
// refresh tokens after 100 days of inactivity, so every sync refreshes first
// — the activity keeps the token alive.
func (s *Service) refreshAccessToken(ctx context.Context, cfg ProviderConfig, clientID, clientSecret, refreshToken string) (*tokenResponse, error) {
	return s.tokenRequest(ctx, cfg.TokenURL, clientID, clientSecret, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
	})
}

func (s *Service) tokenRequest(ctx context.Context, tokenURL, clientID, clientSecret string, form url.Values) (*tokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token endpoint returned %d: %s", resp.StatusCode, truncate(body, 200))
	}
	var out tokenResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("token endpoint response not JSON: %w", err)
	}
	if out.AccessToken == "" {
		return nil, errors.New("token endpoint returned no access_token")
	}
	return &out, nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}

// sha256Hex is the deterministic content hash for synthetic connector source
// documents — the provider, the sync date, and the entity type. The doc has
// no bytes to hash, so the hash identifies the (provider, window, type)
// triple instead, which is what duplicate detection needs for a synthetic
// source.
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
