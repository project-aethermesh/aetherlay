package cors

import (
	"net/http"

	"github.com/rs/zerolog/log"
)

// Middleware creates a CORS middleware with explicit configuration values.
func Middleware(next http.Handler, corsHeaders, corsMethods, corsOrigin string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Set CORS headers
		w.Header().Set("Access-Control-Allow-Headers", corsHeaders)
		w.Header().Set("Access-Control-Allow-Methods", corsMethods)
		w.Header().Set("Access-Control-Allow-Origin", corsOrigin)

		// Log the headers and their values for debugging single requests
		log.Debug().Str("Access-Control-Allow-Headers", corsHeaders).Msg("CORS config -")
		log.Debug().Str("Access-Control-Allow-Methods", corsMethods).Msg("CORS config -")
		log.Debug().Str("Access-Control-Allow-Origin", corsOrigin).Msg("CORS config -")

		// Handle preflight OPTIONS request
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		// Continue with the next handler
		next.ServeHTTP(w, r)
	})
}
