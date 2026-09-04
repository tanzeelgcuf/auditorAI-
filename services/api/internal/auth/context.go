package auth

import "context"

// ContextKey is the type of every request-scoped identity value this API puts on
// a context.
//
// WHY THESE LIVE IN `auth` AND NOT IN `middleware`:
// `internal/middleware` imports `internal/auth` (it needs *auth.Service to
// validate a JWT), so `auth` importing `middleware` is an import cycle and will
// not build. That left `auth`'s own handlers with no way to read a key that
// `middleware` owned, and the workaround shipped was
// `r.Context().Value("user_id")` with an UNTYPED string literal. An untyped
// `string` key never matches a `contextKey("user_id")` write — different dynamic
// type, so `Value` returns nil — which meant HandleEnableTOTP and
// HandleVerifyTOTP returned 401 unconditionally for every caller, including
// correctly authenticated ones. Defining the keys at the bottom of the
// dependency order and aliasing them upward in `middleware` is what makes that
// class of mismatch impossible rather than merely fixed once.
type ContextKey string

const (
	// UserIDKey holds the authenticated user's UUID (string).
	UserIDKey ContextKey = "user_id"
	// FirmIDKey holds the authenticated user's firm UUID (string).
	FirmIDKey ContextKey = "firm_id"
	// AssignedBooksKey holds the client_book UUIDs the user may see ([]string).
	AssignedBooksKey ContextKey = "assigned_books"
	// RoleKey holds the user's role: "staff" or "firm_admin" (string).
	RoleKey ContextKey = "role"
	// SourceIPKey holds the canonical client address for the request (string),
	// set once by middleware.SourceIP and never re-derived per call site. It is
	// NOT identity — it survives here rather than in `middleware` for the reason
	// in the type comment above: the key has to sit below every package that
	// reads it, and the audit writers that read it are spread across
	// `middleware`, `humanoverride` and `periods`.
	SourceIPKey ContextKey = "source_ip"
)

// UserIDFrom returns the authenticated user ID, or "" when the request did not
// pass through middleware.Authenticator. Callers MUST treat "" as unauthorized
// rather than as a missing-but-harmless value: every handler that uses it scopes
// a database write by it.
func UserIDFrom(ctx context.Context) string {
	if v, ok := ctx.Value(UserIDKey).(string); ok {
		return v
	}
	return ""
}

// FirmIDFrom returns the authenticated user's firm ID, or "" if absent.
func FirmIDFrom(ctx context.Context) string {
	if v, ok := ctx.Value(FirmIDKey).(string); ok {
		return v
	}
	return ""
}

// RoleFrom returns the authenticated user's role, or "" if absent.
func RoleFrom(ctx context.Context) string {
	if v, ok := ctx.Value(RoleKey).(string); ok {
		return v
	}
	return ""
}

// SourceIPFrom returns the client address recorded for this request, or "" when
// the request did not pass through middleware.SourceIP (background workers, CLI
// tools, and unit tests that build a bare context).
//
// "" is a legitimate answer and callers must persist it as SQL NULL rather than
// as the string "" — an audit row that claims the request came from nowhere is
// worse than one that admits it does not know. Every INSERT that stores this
// uses NULLIF($n, '')::inet for exactly that reason.
func SourceIPFrom(ctx context.Context) string {
	if v, ok := ctx.Value(SourceIPKey).(string); ok {
		return v
	}
	return ""
}
