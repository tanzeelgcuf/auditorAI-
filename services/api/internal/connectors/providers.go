// The provider configs. Both providers are OAuth2 authorization-code flows;
// the scopes are the minimal read sets for reconciliation. The connector
// READS from the provider and never writes to its books — it is not the
// system of record (the architecture's first corollary), so no write scopes
// are requested.
package connectors

import (
	"errors"
	"os"
)

type Provider string

const (
	ProviderQuickBooks Provider = "quickbooks"
	ProviderXero       Provider = "xero"
)

type ProviderConfig struct {
	AuthorizeURL string
	TokenURL     string
	APIBase      string
	Scopes       []string
}

// configFor returns the provider's OAuth2 and API configuration. The API
// bases default to the SANDBOX hosts (dev posture) and are overridden by
// QBO_API_BASE / XERO_API_BASE for production; a scope set that includes
// writes would be a scope-lock question, so none is requested.
func configFor(p Provider) (ProviderConfig, error) {
	switch p {
	case ProviderQuickBooks:
		// QuickBooks Online v3: authorize at appcenter.intuit.com, tokens
		// refresh at oauth.platform.intuit.com, the API is realm-scoped
		// /v3/company/{realmId}. Rate limits: 500 req/min per company, 10
		// concurrent connections.
		base := os.Getenv("QBO_API_BASE")
		if base == "" {
			base = "https://sandbox-quickbooks.api.intuit.com/v3/company"
		}
		return ProviderConfig{
			AuthorizeURL: "https://appcenter.intuit.com/connect/oauth2",
			TokenURL:     "https://oauth.platform.intuit.com/oauth2/v1/tokens/bearer",
			APIBase:      base,
			Scopes:       []string{"com.intuit.quickbooks.accounting"},
		}, nil
	case ProviderXero:
		// Xero: authorize at login.xero.com/identity/connect/authorize, tokens
		// refresh at identity.xero.com/connect/token, the API is
		// api.xero.com with every call carrying the Xero-tenant-id header.
		base := os.Getenv("XERO_API_BASE")
		if base == "" {
			base = "https://api.xero.com"
		}
		return ProviderConfig{
			AuthorizeURL: "https://login.xero.com/identity/connect/authorize",
			TokenURL:     "https://identity.xero.com/connect/token",
			APIBase:      base,
			Scopes: []string{
				"openid", "profile", "email",
				"accounting.transactions", "accounting.contacts",
				"offline_access",
			},
		}, nil
	}
	return ProviderConfig{}, errors.New("unknown provider")
}
