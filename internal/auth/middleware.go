package auth

import (
	"net/http"
	"os"
)

const (
	clientTokenEnv = "SYMKERNEL_CLIENT_TOKEN"
	bearerPrefix   = "Bearer "
)

// Middleware requires Authorization: Bearer <token> to match SYMKERNEL_CLIENT_TOKEN.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		expected := os.Getenv(clientTokenEnv)
		got := r.Header.Get("Authorization")

		if expected == "" || got != bearerPrefix+expected {
			http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r)
	})
}
