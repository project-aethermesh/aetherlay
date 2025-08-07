package metrics

import (
	"fmt"
	"net/http"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog/log"
)

// StartServer starts a dedicated HTTP server in a goroutine to expose metrics.
func StartServer(port int) {
	go func() {
		// Create a new router for the metrics server
		mux := http.NewServeMux()
		// Expose the /metrics endpoint using the promhttp handler
		mux.Handle("/metrics", promhttp.Handler())

		addr := fmt.Sprintf(":%d", port)
		log.Info().Str("address", addr).Msg("Starting metrics server")

		// Start the HTTP server
		if err := http.ListenAndServe(addr, mux); err != nil {
			log.Fatal().Err(err).Msg("Metrics server failed to start")
		}
	}()
}
