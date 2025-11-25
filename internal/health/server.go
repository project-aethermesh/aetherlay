package health

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// HealthCheckerServer provides HTTP endpoints for health and readiness checks
type HealthCheckerServer struct {
	checker    *Checker
	checkerMu  sync.RWMutex // Protects checker field from concurrent access
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

// SetChecker updates the checker instance after the server has started
// This allows the server to start before dependencies (config, Valkey) are available
func (s *HealthCheckerServer) SetChecker(checker *Checker) {
	s.checkerMu.Lock()
	defer s.checkerMu.Unlock()
	s.checker = checker
	log.Info().Msg("Health checker HTTP server: checker instance updated")
}

// Start starts the HTTP server in a goroutine and reports bind errors via the error channel
func (s *HealthCheckerServer) Start(startupErrCh chan<- error) {
	log.Info().Str("addr", s.httpServer.Addr).Msg("Starting health checker HTTP server")
	go func() {
		log.Debug().Str("addr", s.httpServer.Addr).Msg("HTTP server goroutine started, attempting to bind")
		// First attempt to bind to detect immediate errors (port in use, permission denied, etc.)
		listener, err := net.Listen("tcp", s.httpServer.Addr)
		if err != nil {
			log.Error().Err(err).Str("addr", s.httpServer.Addr).Msg("Failed to bind to address")
			startupErrCh <- err
			return
		}

		log.Info().Str("addr", s.httpServer.Addr).Msg("Successfully bound to address, starting HTTP server")
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
	w.WriteHeader(http.StatusOK)

	response := map[string]any{
		"status": "healthy",
	}
	if err := json.NewEncoder(w).Encode(response); err != nil {
		log.Error().Err(err).Msg("Failed to encode health response")
	}
}

// handleReady handles the /ready endpoint for readiness checks
// Returns 200 only after initial health checks complete, 503 otherwise
func (s *HealthCheckerServer) handleReady(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	var response map[string]any
	var statusCode int

	// Read ready state under lock, then unlock before I/O
	s.checkerMu.RLock()
	checker := s.checker
	s.checkerMu.RUnlock()

	var isReady bool
	if checker != nil {
		isReady = checker.IsReady()
	} else {
		isReady = false
	}

	if !isReady {
		response = map[string]any{
			"status": "not_ready",
			"reason": "initial_health_check_in_progress",
		}
		statusCode = http.StatusServiceUnavailable
	} else {
		response = map[string]any{
			"status": "ready",
		}
		statusCode = http.StatusOK
	}

	w.WriteHeader(statusCode)
	if err := json.NewEncoder(w).Encode(response); err != nil {
		log.Error().Err(err).Msg("Failed to encode ready response")
	}
}
