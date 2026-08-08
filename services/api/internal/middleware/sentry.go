package middleware

import (
	"fmt"
	"net/http"

	"github.com/getsentry/sentry-go"
)

// SentryRecoverer captures panics to Sentry (no-op when no DSN configured),
// then re-panics so chi's middleware.Recoverer still renders the 500 response.
// Mount BEFORE Recoverer in the middleware chain.
func SentryRecoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				hub := sentry.CurrentHub().Clone()
				hub.ConfigureScope(func(scope *sentry.Scope) {
					scope.SetRequest(r)
				})
				hub.CaptureException(fmt.Errorf("panic: %v", err))
				panic(err)
			}
		}()
		next.ServeHTTP(w, r)
	})
}
