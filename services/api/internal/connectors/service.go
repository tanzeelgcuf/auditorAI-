// The handlers. HandleAuthorize and HandleList sit behind the authenticator
// (the assigned-books gate). HandleCallback is PUBLIC — the provider's
// redirect lands here with no JWT, so the signed `state` parameter IS its
// authentication, the same posture as the Stripe webhook: the caller cannot
// present a JWT and the handler authenticates its own caller.
package connectors

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/middleware"
	"github.com/tanzeelgcuf/ai-auditor/services/api/internal/pipeline"
)

type Service struct {
	db          *pgxpool.Pool // the app pool — the authenticated reads
	sysDB       *pgxpool.Pool // the sys pool — the public callback's write (BYPASSRLS, cross-firm by design)
	stateSecret []byte
	http        *http.Client
	pipeline    *pipeline.EventClient // the sync publishes the same pipeline events
}

func NewService() *Service {
	return &Service{http: &http.Client{Timeout: 30 * time.Second}}
}

func (s *Service) SetDB(db *pgxpool.Pool)              { s.db = db }
func (s *Service) SetSysDB(db *pgxpool.Pool)           { s.sysDB = db }
func (s *Service) SetStateSecret(secret []byte)        { s.stateSecret = secret }
func (s *Service) SetPipeline(p *pipeline.EventClient) { s.pipeline = p }

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeProblem(w http.ResponseWriter, status int, typ, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"type": typ, "title": http.StatusText(status), "status": status, "detail": detail,
	})
}

// credentialsFor reads the provider's OAuth client credentials. A missing
// credential is 503 — the same posture as the Stripe webhook's unset secret:
// the connector cannot function and the message must say why.
func credentialsFor(p Provider) (string, string, error) {
	switch p {
	case ProviderQuickBooks:
		return os.Getenv("QBO_CLIENT_ID"), os.Getenv("QBO_CLIENT_SECRET"), nil
	case ProviderXero:
		return os.Getenv("XERO_CLIENT_ID"), os.Getenv("XERO_CLIENT_SECRET"), nil
	}
	return "", "", errors.New("unknown provider")
}

// redirectBase is where the provider sends the browser back. The redirect_uri
// registered at the provider must match EXACTLY, so it is one env value
// (CONNECTOR_REDIRECT_BASE, default http://localhost:8080) — a mismatch is
// the most common OAuth integration failure and should be diagnosable from
// one place.
func redirectBase() string {
	if v := os.Getenv("CONNECTOR_REDIRECT_BASE"); v != "" {
		return v
	}
	return "http://localhost:8080"
}

// HandleAuthorize redirects the user's browser to the provider's consent
// screen. Behind the authenticator; the assigned-books gate matches every
// other /v1/books route. No DB read: the signed state carries everything the
// callback needs.
func (s *Service) HandleAuthorize(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("bookId")
	provider := Provider(r.PathValue("provider"))
	userID := middleware.GetUserID(r.Context())

	assigned := middleware.GetAssignedBooks(r.Context())
	found := false
	for _, b := range assigned {
		if b == bookID {
			found = true
			break
		}
	}
	if bookID == "" || !found {
		writeProblem(w, http.StatusNotFound, "https://ai-auditor.dev/errors/not-found", "book not found")
		return
	}
	if userID == "" {
		writeProblem(w, http.StatusUnauthorized, "https://ai-auditor.dev/errors/unauthorized", "unauthorized")
		return
	}

	cfg, err := configFor(provider)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "https://ai-auditor.dev/errors/bad-request", "unknown provider")
		return
	}
	clientID, _, err := credentialsFor(provider)
	if err != nil || clientID == "" {
		writeProblem(w, http.StatusServiceUnavailable, "https://ai-auditor.dev/errors/not-configured",
			"connector not configured: OAuth client credentials missing")
		return
	}

	redirectURI := redirectBase() + "/v1/connectors/" + string(provider) + "/callback"
	state := s.signOAuthState(userID, bookID, string(provider), time.Now().Add(oauthStateTTL))

	q := url.Values{}
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("response_type", "code")
	q.Set("scope", strings.Join(cfg.Scopes, " "))
	q.Set("state", state)
	http.Redirect(w, r, cfg.AuthorizeURL+"?"+q.Encode(), http.StatusFound)
}

// HandleCallback is the OAuth redirect target, PUBLIC. The signed state IS
// its authentication: verified for signature, expiry, and the exact
// user/book/provider triple before anything else runs. The code is exchanged
// for a token pair, the tokens are encrypted at rest, and the connection row
// is upserted on the sys pool (no JWT exists here, so no RLS priming; the row
// is scoped to the state's book).
func (s *Service) HandleCallback(w http.ResponseWriter, r *http.Request) {
	provider := Provider(r.PathValue("provider"))
	q := r.URL.Query()
	code := q.Get("code")
	state := q.Get("state")
	if code == "" || state == "" {
		writeProblem(w, http.StatusBadRequest, "https://ai-auditor.dev/errors/bad-request", "code and state required")
		return
	}

	userID, bookID, ok := s.verifyOAuthState(state, string(provider))
	if !ok {
		writeProblem(w, http.StatusBadRequest, "https://ai-auditor.dev/errors/bad-request",
			"invalid or expired state — restart the connection from the authorize link")
		return
	}
	_ = userID // the state's subject; the connection is scoped to the book

	cfg, err := configFor(provider)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "https://ai-auditor.dev/errors/bad-request", "unknown provider")
		return
	}
	clientID, clientSecret, err := credentialsFor(provider)
	if err != nil || clientID == "" {
		writeProblem(w, http.StatusServiceUnavailable, "https://ai-auditor.dev/errors/not-configured",
			"connector not configured: OAuth client credentials missing")
		return
	}

	redirectURI := redirectBase() + "/v1/connectors/" + string(provider) + "/callback"
	tok, err := s.exchangeCode(r.Context(), cfg, clientID, clientSecret, code, redirectURI)
	if err != nil {
		slog.Error("oauth token exchange failed", "provider", string(provider), "error", err)
		writeProblem(w, http.StatusBadGateway, "https://ai-auditor.dev/errors/upstream", "token exchange failed")
		return
	}

	accountID := q.Get("realmId") // QuickBooks puts the realmId in the callback
	if provider == ProviderXero {
		// Xero does not put the tenant in the callback; the connections API
		// lists the tenants the token can see.
		id, terr := s.xeroTenantID(r.Context(), cfg, tok.AccessToken)
		if terr != nil {
			slog.Error("xero tenant lookup failed", "error", terr)
			writeProblem(w, http.StatusBadGateway, "https://ai-auditor.dev/errors/upstream", "tenant lookup failed")
			return
		}
		accountID = id
	}
	if accountID == "" {
		writeProblem(w, http.StatusBadRequest, "https://ai-auditor.dev/errors/bad-request", "no provider account id in callback")
		return
	}

	encAccess, err := encryptToken(tok.AccessToken)
	if err != nil {
		slog.Error("token encryption failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "token encryption failed")
		return
	}
	encRefresh, err := encryptToken(tok.RefreshToken)
	if err != nil {
		slog.Error("token encryption failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "token encryption failed")
		return
	}

	var expiresAt *time.Time
	if tok.ExpiresIn > 0 {
		t := time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
		expiresAt = &t
	}

	if _, err := s.sysDB.Exec(r.Context(),
		`INSERT INTO connector_connections
			(client_book_id, provider, provider_account_id, encrypted_access_token, encrypted_refresh_token, token_expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (client_book_id, provider)
		 DO UPDATE SET provider_account_id = EXCLUDED.provider_account_id,
			encrypted_access_token = EXCLUDED.encrypted_access_token,
			encrypted_refresh_token = EXCLUDED.encrypted_refresh_token,
			token_expires_at = EXCLUDED.token_expires_at`,
		bookID, string(provider), accountID, encAccess, encRefresh, expiresAt); err != nil {
		slog.Error("connector upsert failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "connection persist failed")
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"client_book_id": bookID, "provider": string(provider), "connected": true,
	})
}

// xeroTenantID returns the tenantId the token can see. Xero's callback does
// not carry the tenant; the connections API lists the tenants granted.
func (s *Service) xeroTenantID(ctx context.Context, cfg ProviderConfig, accessToken string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", cfg.APIBase+"/connections", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := s.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", errors.New("connections endpoint returned " + strconv.Itoa(resp.StatusCode) + ": " + truncate(body, 200))
	}
	var tenants []struct {
		TenantID   string `json:"tenantId"`
		TenantName string `json:"tenantName"`
	}
	if err := json.Unmarshal(body, &tenants); err != nil {
		return "", errors.New("connections response not JSON")
	}
	if len(tenants) == 0 {
		return "", errors.New("token grants access to no Xero tenants")
	}
	return tenants[0].TenantID, nil
}

// HandleList returns the book's connector connections — metadata only, never
// the tokens. Behind the authenticator; the read runs on the request's
// RLS-primed connection (rule 14).
func (s *Service) HandleList(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("bookId")
	assigned := middleware.GetAssignedBooks(r.Context())
	found := false
	for _, b := range assigned {
		if b == bookID {
			found = true
			break
		}
	}
	if bookID == "" || !found {
		writeProblem(w, http.StatusNotFound, "https://ai-auditor.dev/errors/not-found", "book not found")
		return
	}

	c := middleware.GetConn(r.Context())
	if c == nil {
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "no db conn")
		return
	}

	rows, err := c.Query(r.Context(),
		`SELECT id::text, provider, provider_account_id,
			COALESCE(token_expires_at::text, ''), COALESCE(last_synced_at::text, ''), created_at
		 FROM connector_connections
		 WHERE client_book_id = $1`, bookID)
	if err != nil {
		slog.Error("failed to list connectors", "error", err)
		writeProblem(w, http.StatusInternalServerError, "https://ai-auditor.dev/errors/internal", "query failed")
		return
	}
	defer rows.Close()

	type conn struct {
		ID              string    `json:"id"`
		Provider        string    `json:"provider"`
		ProviderAccount string    `json:"provider_account_id"`
		TokenExpires    string    `json:"token_expires_at"`
		LastSynced      string    `json:"last_synced_at"`
		CreatedAt       time.Time `json:"created_at"`
	}
	out := []conn{}
	for rows.Next() {
		var e conn
		if err := rows.Scan(&e.ID, &e.Provider, &e.ProviderAccount, &e.TokenExpires, &e.LastSynced, &e.CreatedAt); err != nil {
			continue
		}
		out = append(out, e)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"items": out})
}
