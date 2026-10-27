package server

import (
	"crypto/subtle"
	"errors"
	"net/http"

	"focus-test-runtime/internal/runtime"
)

// Authorized is the production entry point. Every route except health requires
// the local owner's credential; no credential appears in startup output.
func Authorized(service *runtime.Service, token string) http.Handler {
	inner := New(service)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			expected := "Bearer " + token
			if len(token) < 26 || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte(expected)) != 1 {
				writeError(w, 401, "unauthorized", errors.New("local runtime credential required"))
				return
			}
		}
		inner.ServeHTTP(w, r)
	})
}
