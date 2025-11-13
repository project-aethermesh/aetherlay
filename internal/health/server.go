package health

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strconv"

	"github.com/rs/zerolog/log"
)

// HealthCheckerServer provides HTTP endpoints for health and readiness checks
type HealthCheckerServer struct {
	checker    *Checker
	httpServer *http.Server
}

// NewHealthCheckerServer creates a new health checker HTTP server
func NewHealthCheckerServer(port int, checker *Checker) *HealthCheckerServer {
	mux := http.NewServeMux()
	server := &HealthCheckerServer{
		checker: checker,
		httpServer: &http.Server{
			Addr:    ":" + strconv.Itoa(port),
			Handler: mux,
		},
	}

	// Register endpoints
	mux.HandleFunc("/health", server.handleHealth)
	mux.HandleFunc("/ready", server.handleReady)

	return server
}

// Start starts the HTTP server in a goroutine and reports bind errors via the error channel
func (s *HealthCheckerServer) Start(startupErrCh chan<- error) {
	log.Info().Str("addr", s.httpServer.Addr).Msg("Starting health checker HTTP server")
	go func() {
		// First attempt to bind to detect immediate errors (port in use, permission denied, etc.)
		listener, err := net.Listen("tcp", s.httpServer.Addr)
		if err != nil {
			log.Error().Err(err).Str("addr", s.httpServer.Addr).Msg("Failed to bind to address")
			startupErrCh <- err
			return
		}

		// Successfully bound, send nil to indicate successful startup
		startupErrCh <- nil

		// Now serve on the listener
		if err := s.httpServer.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.Fatal().Err(err).Msg("Health checker HTTP server failed")
		}
	}()
}

// Shutdown gracefully shuts down the HTTP server
func (s *HealthCheckerServer) Shutdown(ctx context.Context) error {
	log.Info().Msg("Shutting down health checker HTTP server")
	return s.httpServer.Shutdown(ctx)
}

// handleHealth handles the /health endpoint for liveness checks
// Always returns 200 to indicate the process is alive
func (s *HealthCheckerServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "healthy",
	})
}

// handleReady handles the /ready endpoint for readiness checks
// Returns 200 only after initial health checks complete, 503 otherwise
func (s *HealthCheckerServer) handleReady(w http.ResponseWriter, r *http.Request) {
	if s.checker != nil && !s.checker.IsReady() {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status": "not_ready",
			"reason": "initial_health_check_in_progress",
		})
		return
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "ready",
	})
}
