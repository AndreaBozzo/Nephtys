package server

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// bearerAuth returns middleware that validates Bearer token authentication.
// If token is empty, all protected endpoints return 403 (auth not configured).
// Public paths (e.g. /health) are exempt and always pass through.
func bearerAuth(token string, publicPaths map[string]bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if publicPaths[r.URL.Path] {
				next.ServeHTTP(w, r)
				return
			}

			if token == "" {
				writeJSON(w, http.StatusForbidden, map[string]string{
					"error":   "forbidden",
					"message": "Admin endpoints are disabled (no NEPHTYS_ADMIN_TOKEN configured)",
				})
				return
			}

			auth := r.Header.Get("Authorization")
			const scheme = "Bearer "
			if len(auth) < len(scheme) || !strings.EqualFold(auth[:len(scheme)], scheme) {
				writeJSON(w, http.StatusUnauthorized, map[string]string{
					"error":   "unauthorized",
					"message": "Missing or invalid Authorization header. Expected: Bearer <token>",
				})
				return
			}

			provided := auth[len(scheme):]
			if subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
				writeJSON(w, http.StatusUnauthorized, map[string]string{
					"error":   "unauthorized",
					"message": "Missing or invalid Authorization header. Expected: Bearer <token>",
				})
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
