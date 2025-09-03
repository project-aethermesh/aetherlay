package cors

import (
	"net/http"
)

// Middleware creates a CORS middleware with explicit configuration values.
func Middleware(next http.Handler, corsHeaders, corsMethods, corsOrigin string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Set the CORS headers if not already set
		if w.Header().Get("Access-Control-Allow-Headers") == "" {
			w.Header().Set("Access-Control-Allow-Headers", corsHeaders)
		}
		if w.Header().Get("Access-Control-Allow-Methods") == "" {
			w.Header().Set("Access-Control-Allow-Methods", corsMethods)
		}
		if w.Header().Get("Access-Control-Allow-Origin") == "" {
			w.Header().Set("Access-Control-Allow-Origin", corsOrigin)
		}

		// Handle preflight OPTIONS request
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		// Continue with the next handler
		next.ServeHTTP(w, r)
	})
}
