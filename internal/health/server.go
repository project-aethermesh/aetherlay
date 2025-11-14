package health

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"time"

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
			Addr:              ":" + strconv.Itoa(port),
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       5 * time.Second,
			WriteTimeout:      5 * time.Second,
			IdleTimeout:       10 * time.Second,
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
	w.Header().Set("Content-Type", "application/json")

	response := map[string]interface{}{
		"status": "healthy",
	}
	body, err := json.Marshal(response)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		errorBody, _ := json.Marshal(map[string]string{"error": "failed to encode response"})
		w.Write(errorBody)
		log.Error().Err(err).Msg("Failed to marshal health response")
		return
	}

	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(body); err != nil {
		log.Error().Err(err).Msg("Failed to write health response")
	}
}

// handleReady handles the /ready endpoint for readiness checks
// Returns 200 only after initial health checks complete, 503 otherwise
func (s *HealthCheckerServer) handleReady(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	var response map[string]interface{}
	var statusCode int

	if s.checker != nil && !s.checker.IsReady() {
		response = map[string]interface{}{
			"status": "not_ready",
			"reason": "initial_health_check_in_progress",
		}
		statusCode = http.StatusServiceUnavailable
	} else {
		response = map[string]interface{}{
			"status": "ready",
		}
		statusCode = http.StatusOK
	}

	body, err := json.Marshal(response)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		errorBody, _ := json.Marshal(map[string]string{"error": "failed to encode response"})
		w.Write(errorBody)
		log.Error().Err(err).Msg("Failed to marshal ready response")
		return
	}

	w.WriteHeader(statusCode)
	if _, err := w.Write(body); err != nil {
		log.Error().Err(err).Msg("Failed to write ready response")
	}
}
