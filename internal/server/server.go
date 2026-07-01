package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"aetherlay/internal/cache"
	"aetherlay/internal/config"
	"aetherlay/internal/health"
	"aetherlay/internal/helpers"
	"aetherlay/internal/metrics"
	"aetherlay/internal/store"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"github.com/rs/zerolog/log"
)

// RateLimitError represents a rate limiting error during WebSocket handshake
type RateLimitError struct {
	StatusCode int
	Message    string
	Signal     health.RateLimitSignal
}

// Error returns the rate limit error message.
func (e *RateLimitError) Error() string {
	return e.Message
}

// BadRequestError represents a 400 Bad Request error that may need special handling
type BadRequestError struct {
	StatusCode int
	Message    string
	Body       []byte
	Headers    http.Header
}

// Error returns the bad request error message.
func (e *BadRequestError) Error() string {
	return e.Message
}

// HealthCheckerIface defines the interface for health checker operations needed by the server
type HealthCheckerIface interface {
	IsReady() bool
}

// endpointFailureState tracks consecutive failures and successes for debouncing
type endpointFailureState struct {
	consecutiveFailures  int
	consecutiveSuccesses int
	lastUpdate           time.Time
	mu                   sync.RWMutex
}

// Server represents the RPC load balancer server
type Server struct {
	appConfig              *helpers.LoadedConfig
	config                 *config.Config
	ephemeralChecksEnabled bool
	healthCache            *cache.HealthCache
	healthChecker          HealthCheckerIface
	httpServer             *http.Server
	maxRetries             int
	rateLimitScheduler     *RateLimitScheduler
	valkeyClient           store.ValkeyClientIface
	requestTimeout         time.Duration
	requestTimeoutPerTry   time.Duration
	router                 *mux.Router

	// Debouncing state tracking
	failureStates    map[string]*endpointFailureState
	failureThreshold int
	successThreshold int
	failureStatesMu  sync.RWMutex

	// Health checker grace period state tracking
	initialCheckPassed bool
	hcFailureTimestamp time.Time
	hcFailureMu        sync.Mutex

	// Reusable HTTP client for health checker readiness checks
	healthCheckClient *http.Client

	forwardRequestWithBody func(w http.ResponseWriter, ctx context.Context, method, targetURL string, bodyBytes []byte, headers http.Header) error
	proxyWebSocket         func(w http.ResponseWriter, r *http.Request, backendURL string) error
}

// NewServer creates a new server instance
func NewServer(cfg *config.Config, valkeyClient store.ValkeyClientIface, appConfig *helpers.LoadedConfig) *Server {
	s := &Server{
		appConfig:              appConfig,
		config:                 cfg,
		ephemeralChecksEnabled: appConfig.EphemeralChecksEnabled,
		failureStates:          make(map[string]*endpointFailureState),
		failureThreshold:       appConfig.EndpointFailureThreshold,
		healthCache:            cache.NewHealthCache(time.Duration(appConfig.HealthCacheTTL) * time.Second),
		maxRetries:             appConfig.ProxyMaxRetries,
		requestTimeout:         time.Duration(appConfig.ProxyTimeout) * time.Second,
		requestTimeoutPerTry:   time.Duration(appConfig.ProxyTimeoutPerTry) * time.Second,
		router:                 mux.NewRouter(),
		successThreshold:       appConfig.EndpointSuccessThreshold,
		valkeyClient:           valkeyClient,
	}

	s.forwardRequestWithBody = s.defaultForwardRequestWithBodyFunc
	s.proxyWebSocket = s.defaultProxyWebSocket

	// Initialize rate limit scheduler
	s.rateLimitScheduler = NewRateLimitScheduler(s.config, valkeyClient)

	// Initialize reusable HTTP client for health checker readiness checks
	transport := &http.Transport{
		MaxIdleConns:        10,
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     30 * time.Second,
	}
	s.healthCheckClient = &http.Client{
		Transport: transport,
		Timeout:   2 * time.Second,
	}

	s.setupRoutes()
	return s
}

// setupRoutes configures the HTTP routes for the server
func (s *Server) setupRoutes() {
	// Health check endpoint
	s.router.HandleFunc("/health", s.handleHealthCheck).Methods("GET")

	// Readiness endpoint (reports ready only after initial health checks complete)
	s.router.HandleFunc("/ready", s.handleReadinessCheck).Methods("GET")

	// Chain-specific endpoints
	for chain := range s.config.Endpoints {
		s.router.HandleFunc("/"+chain, s.handleRequestHTTP(chain)).Methods("POST")
		// Add GET handler for WebSocket upgrade
		s.router.HandleFunc("/"+chain, s.handleRequestWS(chain)).Methods("GET")
		// Add OPTIONS handler for CORS preflight requests
		s.router.HandleFunc("/"+chain, s.handleOptionsRequest).Methods("OPTIONS")
	}
}

// Start starts the HTTP server on the specified port
func (s *Server) Start(port int) error {
	s.httpServer = &http.Server{
		Addr:              ":" + strconv.Itoa(port),
		Handler:           s.router,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       30 * time.Second,
	}

	log.Info().Int("port", port).Msg("Starting server")
	return s.httpServer.ListenAndServe()
}

// Shutdown gracefully shuts down the server
func (s *Server) Shutdown() error {
	log.Info().Msg("Initiating server shutdown...")

	// Shutdown rate limit scheduler first
	if s.rateLimitScheduler != nil {
		if err := s.rateLimitScheduler.Shutdown(10 * time.Second); err != nil {
			log.Warn().Err(err).Msg("Rate limit scheduler shutdown did not complete cleanly")
		}
	}

	// Shutdown HTTP server
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := s.httpServer.Shutdown(ctx); err != nil {
		log.Error().Err(err).Msg("HTTP server shutdown failed")
		return err
	}

	log.Info().Msg("Server shutdown completed")
	return nil
}

// AddMiddleware adds a middleware to the server's router
func (s *Server) AddMiddleware(middleware func(http.Handler) http.Handler) {
	s.router.Use(middleware)
}

// SetHealthChecker sets the health checker for the server
func (s *Server) SetHealthChecker(checker HealthCheckerIface) {
	s.healthChecker = checker
}

// handleHealthCheck handles the /health endpoint for liveness checks
func (s *Server) handleHealthCheck(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	response := map[string]any{
		"status": "healthy",
	}
	if err := json.NewEncoder(w).Encode(response); err != nil {
		log.Error().Err(err).Msg("Failed to encode health response")
	}
}

// writeReadinessResponse writes a JSON readiness response with proper error handling
func (s *Server) writeReadinessResponse(w http.ResponseWriter, statusCode int, status, reason string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)

	response := map[string]any{
		"status": status,
	}
	if reason != "" {
		response["reason"] = reason
	}

	if err := json.NewEncoder(w).Encode(response); err != nil {
		log.Error().Err(err).Msg("Failed to encode readiness response")
	}
}

// writeJSONError writes a JSON error response with the given message and status code.
func writeJSONError(w http.ResponseWriter, message string, statusCode int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	if err := json.NewEncoder(w).Encode(map[string]string{"error": message}); err != nil {
		log.Error().Err(err).Msg("Failed to encode error response")
	}
}

// handleReadinessCheck handles the /ready endpoint for readiness checks
// Returns 200 only when both LB and health-checker are ready
func (s *Server) handleReadinessCheck(w http.ResponseWriter, r *http.Request) {
	log.Debug().Msg("Readiness check started")

	// Check LB's own readiness (Valkey connection)
	log.Debug().Msg("Checking Valkey connection")
	if err := s.valkeyClient.Ping(r.Context()); err != nil {
		log.Warn().Err(err).Msg("Valkey ping failed during readiness check")
		s.writeReadinessResponse(w, http.StatusServiceUnavailable, "not_ready", "valkey_connection_failed")
		return
	}
	log.Debug().Msg("Valkey connection check passed")

	// Check health-checker readiness
	var hcReady bool
	if s.appConfig.StandaloneHealthChecks {
		log.Debug().Str("url", s.appConfig.HealthCheckerServiceURL).Msg("Checking external health-checker service readiness")
		hcReady = s.checkHealthCheckerServiceReady(r.Context())
	} else {
		log.Debug().Msg("Checking integrated health checker readiness")
		hcReady = s.healthChecker != nil && s.healthChecker.IsReady()
	}

	// Handle grace period logic, compute decision under lock, then unlock before I/O
	var statusCode int
	var status, reason string

	s.hcFailureMu.Lock()

	if hcReady {
		// HC is ready, mark initial check as passed and clear failure timestamp
		if !s.initialCheckPassed {
			s.initialCheckPassed = true
			log.Info().Msg("Initial health-checker check passed")
		}
		if !s.hcFailureTimestamp.IsZero() {
			log.Info().Msg("Health checker recovered, clearing failure timestamp")
			s.hcFailureTimestamp = time.Time{}
		}

		if s.appConfig.StandaloneHealthChecks {
			log.Debug().Str("url", s.appConfig.HealthCheckerServiceURL).Msg("Health-checker service is ready")
		} else {
			log.Debug().Msg("Integrated health checker readiness check passed")
		}

		log.Debug().Msg("All readiness checks passed, returning ready")
		statusCode = http.StatusOK
		status = "ready"
		reason = ""
	} else if !s.initialCheckPassed {
		// HC is not ready and no grace period during initial startup
		if s.appConfig.StandaloneHealthChecks {
			log.Warn().Str("url", s.appConfig.HealthCheckerServiceURL).Msg("Health-checker service not ready")
			reason = "health_checker_service_not_ready"
		} else {
			log.Debug().Msg("Integrated health checker not ready yet")
			reason = "initial_health_check_in_progress"
		}
		statusCode = http.StatusServiceUnavailable
		status = "not_ready"
	} else {
		// Initial check has passed, apply grace period
		gracePeriod := time.Duration(s.appConfig.HealthCheckerGracePeriod) * time.Second
		now := time.Now()

		// Set failure timestamp if not already set
		if s.hcFailureTimestamp.IsZero() {
			s.hcFailureTimestamp = now
			log.Info().Dur("grace_period", gracePeriod).Msg("Health checker not ready, starting grace period")
		}

		elapsed := now.Sub(s.hcFailureTimestamp)

		if elapsed < gracePeriod {
			// Still within grace period, report ready
			log.Debug().
				Dur("elapsed", elapsed).
				Dur("grace_period", gracePeriod).
				Dur("remaining", gracePeriod-elapsed).
				Msg("Health checker not ready but within grace period, reporting ready")
			statusCode = http.StatusOK
			status = "ready"
			reason = ""
		} else {
			// Grace period has expired, report not ready
			log.Warn().
				Dur("elapsed", elapsed).
				Dur("grace_period", gracePeriod).
				Msg("Health checker not ready and grace period expired, reporting not ready")
			statusCode = http.StatusServiceUnavailable
			status = "not_ready"
			if s.appConfig.StandaloneHealthChecks {
				reason = "health_checker_service_not_ready"
			} else {
				reason = "health_checker_not_ready"
			}
		}
	}

	s.hcFailureMu.Unlock()

	// Write response after unlocking mutex to avoid blocking concurrent checks
	s.writeReadinessResponse(w, statusCode, status, reason)
}

// checkHealthCheckerServiceReady checks if the external health-checker service is ready
func (s *Server) checkHealthCheckerServiceReady(ctx context.Context) bool {
	// Parse base URL and construct /ready endpoint URL
	baseURL, err := url.Parse(s.appConfig.HealthCheckerServiceURL)
	if err != nil {
		log.Error().Err(err).Str("url", s.appConfig.HealthCheckerServiceURL).Msg("Invalid health checker service URL")
		return false
	}
	checkURL := baseURL.JoinPath("ready").String()
	log.Debug().Str("url", checkURL).Msg("Making HTTP request to health-checker /ready endpoint")

	req, err := http.NewRequestWithContext(ctx, "GET", checkURL, nil)
	if err != nil {
		log.Error().Err(err).Str("url", checkURL).Msg("Failed to create request to health-checker service")
		return false
	}

	startTime := time.Now()
	resp, err := s.healthCheckClient.Do(req)
	duration := time.Since(startTime)

	if err != nil {
		log.Warn().Err(err).Str("url", checkURL).Dur("duration", duration).Msg("Failed to reach health-checker service")
		return false
	}
	defer resp.Body.Close()

	log.Debug().Str("url", checkURL).Int("status_code", resp.StatusCode).Dur("duration", duration).Msg("Health-checker service responded")

	// Health-checker is ready if it returns 200
	if resp.StatusCode == http.StatusOK {
		// Drain response body to enable connection reuse
		io.Copy(io.Discard, resp.Body)
		return true
	}

	// Read response body for debugging
	bodyBytes, readErr := io.ReadAll(resp.Body)
	if readErr == nil {
		log.Debug().Str("url", checkURL).Int("status_code", resp.StatusCode).Str("response_body", string(bodyBytes)).Msg("Health-checker service returned non-200 status")
	} else {
		log.Debug().Str("url", checkURL).Int("status_code", resp.StatusCode).Err(readErr).Msg("Health-checker service returned non-200 status (failed to read body)")
	}

	return false
}

// handleOptionsRequest handles CORS preflight OPTIONS requests
func (s *Server) handleOptionsRequest(w http.ResponseWriter, r *http.Request) {
	// CORS headers are already set by the CORS middleware
	w.WriteHeader(http.StatusOK)
}

// handleRequestHTTP creates a handler for HTTP requests for a specific chain
func (s *Server) handleRequestHTTP(chain string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), s.requestTimeout)
		defer cancel()

		// Check if archive node is requested
		archive := r.URL.Query().Get("archive") == "true"

		// Read and buffer the request body once to avoid "http: invalid Read on closed Body" errors on retries
		var bodyBytes []byte
		if r.Body != nil {
			var err error
			bodyBytes, err = io.ReadAll(r.Body)
			if err != nil {
				writeJSONError(w, "Failed to read request body", http.StatusBadRequest)
				return
			}
			r.Body.Close()
		}

		// Get all available endpoints with public-first logic
		allEndpoints := s.getAvailableEndpoints(chain, archive, false)

		log.Debug().Str("chain", chain).Bool("archive", archive).Int("available_endpoints", len(allEndpoints)).Msg("Retrieved available endpoints for HTTP request")

		if len(allEndpoints) == 0 {
			log.Debug().Str("chain", chain).Bool("archive", archive).Msg("No available endpoints found for HTTP request")
			writeJSONError(w, "No available endpoints", http.StatusServiceUnavailable)
			return
		}

		var triedEndpoints []string
		retryCount := 0
		publicAttemptCount := 0
		var first400Error *BadRequestError
		var first400EndpointID string

		for retryCount < s.maxRetries && len(allEndpoints) > 0 {
			select {
			case <-ctx.Done():
				log.Error().Str("chain", chain).Msg("Request timeout reached")
				writeJSONError(w, "Request timeout", http.StatusGatewayTimeout)
				return
			default:
			}

			// Select the best endpoint based on requests count and endpoint type
			endpoint := s.selectBestEndpoint(chain, allEndpoints)
			if endpoint == nil {
				log.Debug().Str("chain", chain).Int("retry", retryCount).Msg("No suitable endpoint found for HTTP request")
				break
			}

			// Skip public endpoints if we've exceeded the attempt limit
			if s.appConfig.PublicFirst && endpoint.Endpoint.Role == "public" && publicAttemptCount >= s.appConfig.PublicFirstAttempts {
				// Remove this public endpoint and continue
				allEndpoints = removeEndpointByID(allEndpoints, endpoint.ID)
				continue
			}

			// Track public endpoint attempts
			if endpoint.Endpoint.Role == "public" {
				publicAttemptCount++
			}

			log.Debug().Str("chain", chain).Str("endpoint", endpoint.ID).Str("endpoint_url", helpers.RedactAPIKey(endpoint.Endpoint.HTTPURL)).Int("retry", retryCount).Msg("Attempting HTTP request to endpoint")

			// Create per-try timeout context that respects the overall timeout
			tryCtx, tryCancel := context.WithTimeout(ctx, s.requestTimeoutPerTry)

			s.recordCapacityUsage(chain, endpoint.Endpoint, endpoint.ID)

			// Create a fresh request with a new body reader for each retry attempt
			err := s.forwardRequestWithBody(w, tryCtx, r.Method, endpoint.Endpoint.HTTPURL, bodyBytes, r.Header)
			tryCancel() // Always cancel the per-try context

			if err != nil {
				// Check if this is a 400 Bad Request error
				if badReqErr, ok := err.(*BadRequestError); ok {
					if first400Error == nil {
						// First 400 response. Cache it and retry with next endpoint.
						first400Error = badReqErr
						first400EndpointID = endpoint.ID
						log.Debug().Str("endpoint", endpoint.ID).Msg("First endpoint returned 400, will retry with next endpoint")
					} else {
						// Second endpoint also returned 400. This is the user's fault, pass it through.
						log.Debug().Str("first_endpoint", first400EndpointID).Str("second_endpoint", endpoint.ID).Msg("Both endpoints returned 400, passing through to the user.")

						// Copy response headers from the error
						for key, values := range badReqErr.Headers {
							// Skip CORS headers to avoid duplication (we set our own)
							if strings.HasPrefix(key, "Access-Control-") {
								continue
							}
							for _, value := range values {
								w.Header().Add(key, value)
							}
						}
						w.WriteHeader(badReqErr.StatusCode)
						if _, err := w.Write(badReqErr.Body); err != nil {
							log.Debug().Err(err).Msg("Failed to write 400 response body to client")
						}
						return
					}
				}

				log.Debug().Str("error", helpers.RedactAPIKey(err.Error())).Str("endpoint", endpoint.ID).Str("endpoint_url", helpers.RedactAPIKey(endpoint.Endpoint.HTTPURL)).Int("retry", retryCount).Msg("HTTP request failed, will retry with different endpoint")
				triedEndpoints = append(triedEndpoints, endpoint.ID)

				// Remove the failed endpoint from the list
				var remainingEndpoints []EndpointWithID
				for _, ep := range allEndpoints {
					if ep.ID != endpoint.ID {
						remainingEndpoints = append(remainingEndpoints, ep)
					}
				}
				allEndpoints = remainingEndpoints
				retryCount++

				// If we got a 400 from first endpoint, continue retrying
				if first400Error != nil && len(allEndpoints) > 0 && retryCount < s.maxRetries {
					log.Debug().Str("chain", chain).Str("failed_endpoint", endpoint.ID).Int("public_attempt_count", publicAttemptCount).Int("remaining_endpoints", len(allEndpoints)).Int("retry", retryCount).Msg("Retrying HTTP request with different endpoint after 400")
					continue
				}

				if len(allEndpoints) > 0 && retryCount < s.maxRetries {
					log.Debug().Str("chain", chain).Str("failed_endpoint", endpoint.ID).Int("public_attempt_count", publicAttemptCount).Int("remaining_endpoints", len(allEndpoints)).Int("retry", retryCount).Msg("Retrying HTTP request with different endpoint")
					continue
				}
			} else {
				// Success. If we had a cached 400 error, mark that endpoint as unhealthy (confirmed it's actually unhealthy)
				if first400Error != nil {
					log.Debug().Str("endpoint", first400EndpointID).Msg("Second endpoint succeeded, marking first endpoint (that returned 400) as unhealthy")
					s.markEndpointUnhealthyProtocol(chain, first400EndpointID, "http")
					first400Error = nil
					first400EndpointID = ""
				}

				// Increment the request count and track success for debouncing.
				// Use a fresh context to ensure metrics are recorded even if the
				// request context is close to expiring.
				log.Debug().Str("chain", chain).Str("endpoint", endpoint.ID).Str("endpoint_url", helpers.RedactAPIKey(endpoint.Endpoint.HTTPURL)).Int("retry", retryCount).Msg("HTTP request succeeded")
				metricsCtx, metricsCancel := context.WithTimeout(context.Background(), 5*time.Second)
				if err := s.valkeyClient.IncrementRequestCount(metricsCtx, chain, endpoint.ID, "proxy_requests"); err != nil {
					log.Error().Err(err).Str("endpoint", endpoint.ID).Msg("Failed to increment request count")
				}
				metricsCancel()
				if metrics.EndpointProxyRequestsTotal != nil {
					metrics.EndpointProxyRequestsTotal.WithLabelValues(chain, endpoint.ID).Inc()
				}
				// Track success for health debouncing
				s.markEndpointHealthyAttempt(chain, endpoint.ID, "http")
				return
			}
		}

		// If we get here, all retries failed
		if retryCount >= s.maxRetries {
			log.Error().Str("chain", chain).Strs("tried_endpoints", triedEndpoints).Int("max_retries", s.maxRetries).Msg("Max retries reached")
			writeJSONError(w, "Max retries reached, all endpoints unavailable", http.StatusBadGateway)
		} else {
			log.Error().Str("chain", chain).Strs("tried_endpoints", triedEndpoints).Msg("All endpoints failed")
			writeJSONError(w, "Failed to forward request, all endpoints unavailable", http.StatusBadGateway)
		}
	}
}

// handleRequestWS creates a handler for WebSocket requests for a specific chain
func (s *Server) handleRequestWS(chain string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Debug().Str("path", r.URL.Path).Msg("Entered handleRequestWS")
		//for k, v := range r.Header {
		//	log.Debug().Str("header", k).Strs("values", v).Msg("Request header")
		//}

		// Only handle WebSocket upgrade requests (case-insensitive, robust)
		if isWebSocketUpgrade(r) {
			ctx, cancel := context.WithTimeout(r.Context(), s.requestTimeout)
			defer cancel()

			archive := r.URL.Query().Get("archive") == "true"

			// Get all available endpoints with public-first logic
			allEndpoints := s.getAvailableEndpoints(chain, archive, true)

			log.Debug().Str("chain", chain).Bool("archive", archive).Int("available_endpoints", len(allEndpoints)).Msg("Retrieved available endpoints for WebSocket request")

			if len(allEndpoints) == 0 {
				log.Debug().Str("chain", chain).Bool("archive", archive).Msg("No available WebSocket endpoints found")
				writeJSONError(w, "No available WebSocket endpoints", http.StatusServiceUnavailable)
				return
			}

			var triedEndpoints []string
			retryCount := 0
			publicAttemptCount := 0
			var first400Error *BadRequestError
			var first400EndpointID string

			for retryCount < s.maxRetries && len(allEndpoints) > 0 {
				select {
				case <-ctx.Done():
					log.Debug().Str("chain", chain).Msg("WebSocket context done, ending handler without marking failure")
					return
				default:
				}

				// Select the best endpoint based on request counts
				endpoint := s.selectBestEndpoint(chain, allEndpoints)
				if endpoint == nil || endpoint.Endpoint.WSURL == "" {
					log.Debug().Str("chain", chain).Int("retry", retryCount).Msg("No suitable WebSocket endpoint found")
					break
				}

				// Skip public endpoints if we've exceeded the attempt limit
				if s.appConfig.PublicFirst && endpoint.Endpoint.Role == "public" && publicAttemptCount >= s.appConfig.PublicFirstAttempts {
					// Remove this public endpoint and continue
					allEndpoints = removeEndpointByID(allEndpoints, endpoint.ID)
					continue
				}

				// Track public endpoint attempts
				if endpoint.Endpoint.Role == "public" {
					publicAttemptCount++
				}

				log.Debug().Str("chain", chain).Str("endpoint", endpoint.ID).Str("endpoint_url", helpers.RedactAPIKey(endpoint.Endpoint.WSURL)).Int("retry", retryCount).Msg("Attempting WebSocket connection to endpoint")

				// Create per-try timeout context that respects the overall timeout
				tryCtx, tryCancel := context.WithTimeout(ctx, s.requestTimeoutPerTry)
				reqWithCtx := r.WithContext(tryCtx)

				s.recordCapacityUsage(chain, endpoint.Endpoint, endpoint.ID)

				err := s.proxyWebSocket(w, reqWithCtx, endpoint.Endpoint.WSURL)
				tryCancel() // Always cancel the per-try context

				// Handle normal/idle closes and timeouts: do not retry or mark as failure
				if err != nil && isExpectedWSClose(err) {
					log.Debug().
						Err(err).
						Str("endpoint", endpoint.ID).
						Msg("WebSocket closed normally, not counting as failure")
					return
				}

				if err != nil {
					// Check if this is a 400 Bad Request error
					if badReqErr, ok := err.(*BadRequestError); ok {
						if first400Error == nil {
							// First 400 response. Cache it and retry with next endpoint.
							first400Error = badReqErr
							first400EndpointID = endpoint.ID
							log.Debug().Str("endpoint", endpoint.ID).Msg("First WebSocket endpoint returned 400, will retry with next endpoint")
						} else {
							// Second endpoint also returned 400. This is the user's fault, pass it through.
							log.Debug().Str("first_endpoint", first400EndpointID).Str("second_endpoint", endpoint.ID).Msg("Both WebSocket endpoints returned 400, passing through to the user.")

							// Copy response headers from the error
							for key, values := range badReqErr.Headers {
								// Skip CORS headers to avoid duplication (we set our own)
								if strings.HasPrefix(key, "Access-Control-") {
									continue
								}
								for _, value := range values {
									w.Header().Add(key, value)
								}
							}
							w.WriteHeader(badReqErr.StatusCode)
							if _, err := w.Write(badReqErr.Body); err != nil {
								log.Debug().Err(err).Msg("Failed to write 400 response body to client")
							}
							return
						}

						// Remove the failed endpoint from the list
						var remainingEndpoints []EndpointWithID
						for _, ep := range allEndpoints {
							if ep.ID != endpoint.ID {
								remainingEndpoints = append(remainingEndpoints, ep)
							}
						}
						allEndpoints = remainingEndpoints
						retryCount++

						// If we got a 400 from first endpoint, continue retrying
						if len(allEndpoints) > 0 && retryCount < s.maxRetries {
							log.Debug().Str("chain", chain).Str("failed_endpoint", endpoint.ID).Int("public_attempt_count", publicAttemptCount).Int("remaining_endpoints", len(allEndpoints)).Int("retry", retryCount).Msg("Retrying WebSocket with different endpoint after 400")
							continue
						}
						// If no more endpoints, break and handle cached 400 error
						break
					}

					// Check if this is a rate limiting error during handshake (429, or Infura's 402 daily cap)
					if rlErr, ok := err.(*RateLimitError); ok {
						log.Debug().Str("chain", chain).Str("endpoint", endpoint.ID).Int("retry", retryCount).Msg("WebSocket handshake rate limited")
						s.handleRateLimit(chain, endpoint.ID, "ws", rlErr.Signal)
						// Remove the rate-limited endpoint from the list
						var remainingEndpoints []EndpointWithID
						for _, ep := range allEndpoints {
							if ep.ID != endpoint.ID {
								remainingEndpoints = append(remainingEndpoints, ep)
							}
						}
						allEndpoints = remainingEndpoints
						retryCount++
						if len(allEndpoints) > 0 && retryCount < s.maxRetries {
							log.Debug().Str("chain", chain).Str("failed_endpoint", endpoint.ID).Int("public_attempt_count", publicAttemptCount).Int("remaining_endpoints", len(allEndpoints)).Int("retry", retryCount).Msg("Retrying WebSocket with different endpoint after rate limit")
							continue
						}
						// If no more endpoints, break and return error
						if len(allEndpoints) == 0 || retryCount >= s.maxRetries {
							if retryCount >= s.maxRetries {
								log.Error().Str("chain", chain).Strs("tried_endpoints", triedEndpoints).Int("max_retries", s.maxRetries).Msg("WebSocket max retries reached after rate limits")
								writeJSONError(w, "WebSocket max retries reached after rate limits, all endpoints unavailable", http.StatusBadGateway)
							} else {
								log.Error().Str("chain", chain).Strs("tried_endpoints", triedEndpoints).Msg("All WebSocket endpoints rate limited")
								writeJSONError(w, "All WebSocket endpoints rate limited", http.StatusTooManyRequests)
							}
							return
						}
					}

					log.Debug().Err(err).Str("endpoint", endpoint.ID).Str("endpoint_url", helpers.RedactAPIKey(endpoint.Endpoint.WSURL)).Int("retry", retryCount).Msg("WebSocket connection failed, will retry with different endpoint")
					triedEndpoints = append(triedEndpoints, endpoint.ID)

					// Remove the failed endpoint from the list
					var remainingEndpoints []EndpointWithID
					for _, ep := range allEndpoints {
						if ep.ID != endpoint.ID {
							remainingEndpoints = append(remainingEndpoints, ep)
						}
					}
					allEndpoints = remainingEndpoints
					retryCount++

					// If we still have endpoints to try, continue the loop
					if len(allEndpoints) > 0 && retryCount < s.maxRetries {
						log.Debug().Str("chain", chain).Str("failed_endpoint", endpoint.ID).Int("remaining_endpoints", len(allEndpoints)).Int("retry", retryCount).Msg("Retrying WebSocket with different endpoint")
						continue
					}
				} else {
					// Success. If we had a cached 400 error, mark that endpoint as unhealthy (confirmed it's actually unhealthy)
					if first400Error != nil {
						log.Debug().Str("endpoint", first400EndpointID).Msg("Second WebSocket endpoint succeeded, marking first endpoint (that returned 400) as unhealthy")
						s.markEndpointUnhealthyProtocol(chain, first400EndpointID, "ws")
						first400Error = nil
						first400EndpointID = ""
					}

					// Increment the request count and track success for debouncing.
					// Use a fresh context since WebSocket connections are long-lived and
					// the original request context may have expired.
					log.Debug().Str("chain", chain).Str("endpoint", endpoint.ID).Str("endpoint_url", helpers.RedactAPIKey(endpoint.Endpoint.WSURL)).Int("retry", retryCount).Msg("WebSocket connection succeeded")
					metricsCtx, metricsCancel := context.WithTimeout(context.Background(), 5*time.Second)
					if err := s.valkeyClient.IncrementRequestCount(metricsCtx, chain, endpoint.ID, "proxy_requests"); err != nil {
						log.Error().Err(err).Str("endpoint", endpoint.ID).Msg("Failed to increment WebSocket request count")
					}
					metricsCancel()
					if metrics.EndpointProxyRequestsTotal != nil {
						metrics.EndpointProxyRequestsTotal.WithLabelValues(chain, endpoint.ID).Inc()
					}
					// Track success for health debouncing
					s.markEndpointHealthyAttempt(chain, endpoint.ID, "ws")
					return
				}
			}

			// If we get here, all retries failed
			if retryCount >= s.maxRetries {
				log.Error().Str("chain", chain).Strs("tried_endpoints", triedEndpoints).Int("max_retries", s.maxRetries).Msg("WebSocket max retries reached")
				writeJSONError(w, "WebSocket max retries reached, all endpoints unavailable", http.StatusBadGateway)
			} else {
				log.Error().Str("chain", chain).Strs("tried_endpoints", triedEndpoints).Msg("All WebSocket endpoints failed")
				writeJSONError(w, "Failed to proxy WebSocket, all endpoints unavailable", http.StatusBadGateway)
			}
			return
		}
		writeJSONError(w, "GET requests to this endpoint are only supported for WebSocket upgrade requests. Otherwise, please use POST.", http.StatusBadRequest)
	}
}

// isWebSocketUpgrade checks if the request is a WebSocket upgrade (case-insensitive, robust)
func isWebSocketUpgrade(r *http.Request) bool {
	conn := r.Header.Get("Connection")
	upg := r.Header.Get("Upgrade")
	return containsToken(conn, "upgrade") && containsToken(upg, "websocket")
}

// containsToken checks if a comma-separated header contains a token (case-insensitive, exact match).
// It splits the header value, trims whitespace, and compares each token to the target using strings.EqualFold.
func containsToken(headerVal, token string) bool {
	for _, part := range splitAndTrim(headerVal) {
		if strings.EqualFold(part, token) {
			return true
		}
	}
	return false
}

// splitAndTrim splits a comma-separated string and trims whitespace from each part.
func splitAndTrim(s string) []string {
	parts := strings.Split(s, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

// EndpointWithID represents an endpoint along with its ID (map key)
type EndpointWithID struct {
	ID       string
	Endpoint config.Endpoint
}

// getAvailableEndpoints returns available endpoints for a chain and protocol with support for public-first hierarchy
func (s *Server) getAvailableEndpoints(chain string, archive bool, ws bool) []EndpointWithID {
	var endpoints []EndpointWithID

	chainEndpoints, exists := s.config.GetEndpointsForChain(chain)
	if !exists {
		return endpoints
	}

	// Get all endpoint types
	publicEndpoints := s.getEndpointsByRole(chainEndpoints, "public", chain, archive, ws)
	primaryEndpoints := s.getEndpointsByRole(chainEndpoints, "primary", chain, archive, ws)
	fallbackEndpoints := s.getEndpointsByRole(chainEndpoints, "fallback", chain, archive, ws)

	// Append endpoints in priority order based on PUBLIC_FIRST setting
	if s.appConfig.PublicFirst {
		// Public-first hierarchy: public → primary → fallback
		endpoints = append(endpoints, publicEndpoints...)
		endpoints = append(endpoints, primaryEndpoints...)
		endpoints = append(endpoints, fallbackEndpoints...)
		log.Debug().Str("chain", chain).Int("1_public", len(publicEndpoints)).Int("2_primary", len(primaryEndpoints)).Int("3_fallback", len(fallbackEndpoints)).Msg("Organized endpoints with PUBLIC_FIRST enabled")
	} else {
		// Normal hierarchy: primary → fallback → public
		endpoints = append(endpoints, primaryEndpoints...)
		endpoints = append(endpoints, fallbackEndpoints...)
		endpoints = append(endpoints, publicEndpoints...)
		log.Debug().Str("chain", chain).Int("1_primary", len(primaryEndpoints)).Int("2_fallback", len(fallbackEndpoints)).Int("3_public", len(publicEndpoints)).Msg("Organized endpoints with normal priority")
	}

	return endpoints
}

// getEndpointsByRole returns healthy endpoints for a specific role
func (s *Server) getEndpointsByRole(chainEndpoints config.ChainEndpoints, role string, chain string, archive bool, ws bool) []EndpointWithID {
	var endpoints []EndpointWithID

	for endpointID, endpoint := range chainEndpoints {
		if endpoint.Role == role {
			if !archive || (archive && endpoint.Type == "archive") {
				// Try to get status from cache first
				status, cacheHit := s.healthCache.Get(chain, endpointID)
				if !cacheHit {
					// Cache miss, fetch from Valkey and populate cache
					var err error
					status, err = s.valkeyClient.GetEndpointStatus(context.Background(), chain, endpointID)
					if err != nil {
						continue
					}
					s.healthCache.Set(chain, endpointID, status)
				}

				// Check if endpoint is rate limited
				rateLimitState, err := s.valkeyClient.GetRateLimitState(context.Background(), chain, endpointID)
				if err == nil && rateLimitState.RateLimited {
					log.Debug().Str("chain", chain).Str("endpoint", endpointID).Str("role", role).Msg("Skipping rate-limited endpoint")
					continue
				}

				// Proactively skip an endpoint that has hit its capacity ceiling (static
				// or learned) for the current window, so selection routes to another
				// endpoint before the provider's own rate limiter would trigger.
				// Independent of RateLimited above: this is a self-imposed budget, not
				// a provider signal.
				if s.appConfig.CapacityThrottlingEnabled {
					if maxRequests, windowSeconds, hasCeiling := s.effectiveCapacityCeiling(chain, endpointID, endpoint); hasCeiling {
						capCtx, capCancel := context.WithTimeout(context.Background(), 2*time.Second)
						count, err := s.valkeyClient.GetCapacityCount(capCtx, chain, endpointID, windowSeconds)
						capCancel()
						if err == nil {
							if metrics.EndpointCapacityUtilization != nil {
								metrics.EndpointCapacityUtilization.WithLabelValues(chain, endpointID).Set(float64(count) / float64(maxRequests))
							}
							if count >= maxRequests {
								log.Debug().Str("chain", chain).Str("endpoint", endpointID).Str("role", role).Msg("Skipping endpoint at its capacity ceiling")
								if metrics.EndpointCapacitySkippedTotal != nil {
									metrics.EndpointCapacitySkippedTotal.WithLabelValues(chain, endpointID).Inc()
								}
								continue
							}
						}
					}
				}

				if ws {
					if status.HasWS && status.HealthyWS {
						endpoints = append(endpoints, EndpointWithID{ID: endpointID, Endpoint: endpoint})
					}
				} else {
					if status.HasHTTP && status.HealthyHTTP {
						endpoints = append(endpoints, EndpointWithID{ID: endpointID, Endpoint: endpoint})
					}
				}
			}
		}
	}

	return endpoints
}

// selectBestEndpoint selects the best endpoint based on endpoint type priority and request counts
func (s *Server) selectBestEndpoint(chain string, endpoints []EndpointWithID) *EndpointWithID {
	if len(endpoints) == 0 {
		return nil
	}

	// Define priority order based on PUBLIC_FIRST setting
	var priorityOrder []string
	if s.appConfig.PublicFirst {
		priorityOrder = []string{"public", "primary", "fallback"}
	} else {
		priorityOrder = []string{"primary", "fallback", "public"}
	}

	// Try each endpoint type in priority order
	for _, role := range priorityOrder {
		bestEndpoint := s.selectBestEndpointByRole(chain, endpoints, role)
		if bestEndpoint != nil {
			return bestEndpoint
		}
	}

	return nil
}

// endpointCeiling caches a resolved capacity ceiling (static or learned) for one
// candidate, so selectBestEndpointByRole doesn't re-resolve it (and re-read Valkey for
// the learned case) once to decide allHaveCeiling and again to compute its score.
type endpointCeiling struct {
	maxRequests   int64
	windowSeconds int
	ok            bool
}

// selectBestEndpointByRole selects the best endpoint of a specific role based on request counts.
// When every candidate endpoint in this role has a resolvable capacity ceiling - static
// Capacity, or a learned estimate once adaptive learning has evidence for it - selection
// is instead weighted by utilization relative to each endpoint's own ceiling - projected
// out to a 24h-equivalent budget so it's comparable to the existing r24h counter - so a
// higher-capacity endpoint isn't penalized for having a higher raw request count than a
// lower-capacity one. If any candidate has no ceiling at all yet, this falls back to the
// original behavior (lowest raw 24h count wins) to avoid comparing endpoints on
// incompatible units.
func (s *Server) selectBestEndpointByRole(chain string, endpoints []EndpointWithID, role string) *EndpointWithID {
	var candidateIndices []int
	ceilings := make(map[int]endpointCeiling)
	allHaveCeiling := true
	for i := range endpoints {
		if endpoints[i].Endpoint.Role != role {
			continue
		}
		candidateIndices = append(candidateIndices, i)
		maxRequests, windowSeconds, ok := s.effectiveCapacityCeiling(chain, endpoints[i].ID, endpoints[i].Endpoint)
		ceilings[i] = endpointCeiling{maxRequests: maxRequests, windowSeconds: windowSeconds, ok: ok}
		if !ok {
			allHaveCeiling = false
		}
	}

	var bestEndpoint *EndpointWithID
	var minScore float64 = -1

	for _, i := range candidateIndices {
		r24h, _, _, err := s.valkeyClient.GetCombinedRequestCounts(context.Background(), chain, endpoints[i].ID)
		// Skip endpoints where we can't get request count data
		if err != nil {
			continue
		}

		score := float64(r24h)
		if allHaveCeiling {
			c := ceilings[i]
			dailyBudget := float64(c.maxRequests) * (86400.0 / float64(c.windowSeconds))
			if dailyBudget > 0 {
				score = float64(r24h) / dailyBudget
			}
		}

		// Select endpoint with the lowest score (or first one if minScore is uninitialized)
		if minScore == -1 || score < minScore {
			minScore = score
			bestEndpoint = &endpoints[i]
		}
	}

	return bestEndpoint
}

// effectiveCapacityCeiling resolves the ceiling (requests/window) to gate and weight an
// endpoint against, whichever source is authoritative:
//  1. Static Capacity configured -> today's exact values, unchanged - adaptive learning
//     never engages for this endpoint.
//  2. No static Capacity, adaptive learning enabled, and a learned estimate exists ->
//     the estimate's ceiling, grown lazily via store.EffectiveMaxRequests.
//  3. Otherwise -> ok=false: no ceiling at all, never proactively skipped. Absence of
//     evidence isn't evidence of a limit - the endpoint remains fully covered by the
//     independent, reactive RateLimitState check the instant it's actually rate limited.
func (s *Server) effectiveCapacityCeiling(chain, endpointID string, ep config.Endpoint) (maxRequests int64, windowSeconds int, ok bool) {
	if ep.Capacity != nil {
		return int64(ep.Capacity.MaxRequests), ep.Capacity.WindowSeconds, true
	}
	if !s.appConfig.CapacityLearningEnabled {
		return 0, 0, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	estimate, err := s.valkeyClient.GetCapacityEstimate(ctx, chain, endpointID)
	if err != nil || !estimate.HasEstimate {
		return 0, 0, false
	}
	params := config.ResolveCapacityLearning(ep.CapacityLearning)
	return store.EffectiveMaxRequests(*estimate, params, time.Now()), estimate.WindowSeconds, true
}

// removeEndpointByID removes an endpoint from a slice by its ID
func removeEndpointByID(endpoints []EndpointWithID, id string) []EndpointWithID {
	var remaining []EndpointWithID
	for _, ep := range endpoints {
		if ep.ID != id {
			remaining = append(remaining, ep)
		}
	}
	return remaining
}

// updateEndpointHealthState tracks consecutive successes/failures and updates endpoint health status when thresholds are reached
func (s *Server) updateEndpointHealthState(chain, endpointID, protocol string, isSuccess bool) {
	if !s.ephemeralChecksEnabled {
		return
	}

	// Get or create failure state for this endpoint:protocol
	key := chain + ":" + endpointID + ":" + protocol

	s.failureStatesMu.Lock()
	state, exists := s.failureStates[key]
	if !exists {
		state = &endpointFailureState{
			consecutiveFailures:  0,
			consecutiveSuccesses: 0,
			lastUpdate:           time.Now(),
		}
		s.failureStates[key] = state
	}
	s.failureStatesMu.Unlock()

	// Update state counters
	state.mu.Lock()
	if isSuccess {
		state.consecutiveSuccesses++
		state.consecutiveFailures = 0
	} else {
		state.consecutiveFailures++
		state.consecutiveSuccesses = 0
	}
	state.lastUpdate = time.Now()
	currentSuccesses := state.consecutiveSuccesses
	currentFailures := state.consecutiveFailures
	state.mu.Unlock()

	// Determine if threshold is reached
	var thresholdReached bool
	var targetHealthy bool
	var threshold int

	if isSuccess {
		threshold = s.successThreshold
		thresholdReached = currentSuccesses >= threshold
		targetHealthy = true

		log.Debug().
			Str("chain", chain).
			Str("endpoint", endpointID).
			Str("protocol", protocol).
			Int("consecutive_successes", currentSuccesses).
			Int("threshold", threshold).
			Msg("Tracking endpoint success")
	} else {
		threshold = s.failureThreshold
		thresholdReached = currentFailures >= threshold
		targetHealthy = false

		log.Debug().
			Str("chain", chain).
			Str("endpoint", endpointID).
			Str("protocol", protocol).
			Int("consecutive_failures", currentFailures).
			Int("threshold", threshold).
			Msg("Tracking endpoint failure")
	}

	// Only update status if threshold is reached
	if !thresholdReached {
		return
	}

	status, err := s.valkeyClient.GetEndpointStatus(context.Background(), chain, endpointID)
	if err != nil {
		log.Error().Err(err).Str("chain", chain).Str("endpoint", endpointID).Str("protocol", protocol).Msgf("Failed to get endpoint status to mark %s", map[bool]string{true: "healthy", false: "unhealthy"}[targetHealthy])
		return
	}

	// Update protocol-specific health status and check if already in target state
	var alreadyInTargetState bool
	switch protocol {
	case "http":
		alreadyInTargetState = status.HealthyHTTP == targetHealthy
		status.HealthyHTTP = targetHealthy
	case "ws":
		alreadyInTargetState = status.HealthyWS == targetHealthy
		status.HealthyWS = targetHealthy
	default:
		log.Warn().Str("protocol", protocol).Msg("Unknown protocol, can't update endpoint health status")
		return
	}

	if alreadyInTargetState {
		log.Debug().Str("chain", chain).Str("endpoint", endpointID).Str("protocol", protocol).Msgf("Endpoint already marked %s, skipping update", map[bool]string{true: "healthy", false: "unhealthy"}[targetHealthy])
		return
	}

	if err := s.valkeyClient.UpdateEndpointStatus(context.Background(), chain, endpointID, *status); err != nil {
		log.Error().Err(err).Str("chain", chain).Str("endpoint", endpointID).Str("protocol", protocol).Msgf("Failed to update endpoint status to %s", map[bool]string{true: "healthy", false: "unhealthy"}[targetHealthy])
	} else {
		// Invalidate cache after successful write
		s.healthCache.Invalidate(chain, endpointID)
		if targetHealthy {
			log.Info().
				Str("chain", chain).
				Str("endpoint", endpointID).
				Str("protocol", protocol).
				Int("consecutive_successes", currentSuccesses).
				Msg("Marked endpoint as healthy after reaching success threshold")
		} else {
			log.Info().
				Str("chain", chain).
				Str("endpoint", endpointID).
				Str("protocol", protocol).
				Int("consecutive_failures", currentFailures).
				Msg("Marked endpoint as unhealthy after reaching failure threshold")
		}
	}
}

// markEndpointUnhealthyProtocol marks the given endpoint as unhealthy for the specified protocol ("http" or "ws") in Valkey.
func (s *Server) markEndpointUnhealthyProtocol(chain, endpointID, protocol string) {
	s.updateEndpointHealthState(chain, endpointID, protocol, false)
}

// markEndpointHealthyAttempt tracks successful requests and marks endpoint as healthy after reaching success threshold
func (s *Server) markEndpointHealthyAttempt(chain, endpointID, protocol string) {
	s.updateEndpointHealthState(chain, endpointID, protocol, true)
}

// findChainAndEndpointByURL searches the config for an endpoint matching the given URL (HTTPURL or WSURL) and returns the chain and endpoint ID.
func (s *Server) findChainAndEndpointByURL(url string) (chain string, endpointID string, found bool) {
	for chainName, endpoints := range s.config.Endpoints {
		for endpointID, endpoint := range endpoints {
			if endpoint.HTTPURL == url || endpoint.WSURL == url {
				return chainName, endpointID, true
			}
		}
	}
	return "", "", false
}

// defaultForwardRequestWithBodyFunc forwards the request to the target endpoint using buffered body data
func (s *Server) defaultForwardRequestWithBodyFunc(w http.ResponseWriter, ctx context.Context, method, targetURL string, bodyBytes []byte, headers http.Header) error {
	// Create a new body reader from the buffered data
	var bodyReader io.Reader
	if len(bodyBytes) > 0 {
		bodyReader = bytes.NewReader(bodyBytes)
	}

	// Create a new request with the context
	req, err := http.NewRequestWithContext(ctx, method, targetURL, bodyReader)
	if err != nil {
		return err
	}

	// Copy headers
	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}

	// Forward the request. Use the context timeout instead of a fixed timeout.
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		if chain, endpointID, found := s.findChainAndEndpointByURL(targetURL); found {
			s.markEndpointUnhealthyProtocol(chain, endpointID, "http")
		} else {
			log.Warn().Str("url", targetURL).Msg("Failed to find chain and endpoint for failed HTTP endpoint URL, cannot mark it as unhealthy")
		}
		return err
	}
	defer resp.Body.Close()

	// Check for non-2xx status codes, all of them should trigger retries
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		chain, endpointID, found := s.findChainAndEndpointByURL(targetURL)

		// Special handling for 400 Bad Request - defer health decision to caller's retry logic.
		// The HTTP handler caches the first 400 and retries with another endpoint. Only if the
		// second endpoint succeeds will it mark the first endpoint as unhealthy. This prevents
		// marking endpoints unhealthy due to client errors (bad requests) rather than endpoint failures.
		// We return early here to avoid any health marking, allowing the caller to make the decision.
		if resp.StatusCode == 400 {
			// Read response body for logging and passing through
			respBodyBytes, readErr := io.ReadAll(resp.Body)
			if readErr != nil {
				respBodyBytes = []byte{}
			}
			return &BadRequestError{
				StatusCode: resp.StatusCode,
				Message:    fmt.Sprintf("HTTP %d: %s", resp.StatusCode, resp.Status),
				Body:       respBodyBytes,
				Headers:    resp.Header,
			}
		}

		// For all other non-2xx responses (400 already handled above), mark endpoint as unhealthy
		if found {
			sig := health.DetectRateLimit(s.providerForEndpoint(chain, endpointID), resp.StatusCode, resp.Header, nil)
			if sig.IsRateLimited {
				s.markEndpointUnhealthyProtocol(chain, endpointID, "http")
				s.handleRateLimit(chain, endpointID, "http", sig)
				log.Debug().Str("url", helpers.RedactAPIKey(targetURL)).Int("status_code", resp.StatusCode).Bool("daily_quota", sig.IsDailyQuota).Msg("Endpoint returned a rate-limit signal, handling rate limit")
			} else {
				s.markEndpointUnhealthyProtocol(chain, endpointID, "http")
				log.Debug().Str("url", helpers.RedactAPIKey(targetURL)).Int("status_code", resp.StatusCode).Msg("Endpoint returned non-2xx status, marked unhealthy")
			}
		}

		// Return error for all non-2xx responses (400 already handled above)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, resp.Status)
	}

	// For JSON responses from a provider known to need it, buffer (within a bounded size)
	// and scan the body for an embedded rate-limit error before forwarding. Some providers
	// (currently Alchemy, on batch requests) return HTTP 200 with the rate-limit signal
	// only inside the JSON-RPC body - invisible to a status-code-only check. This is
	// scoped to those providers specifically: buffering every 2xx JSON response
	// regardless of provider would delay time-to-first-byte and hold up to
	// maxRateLimitScanBodyBytes in memory per in-flight request, for providers that never
	// exhibit this behavior. Responses from other providers, and oversized bodies from
	// providers that do need the scan, are streamed through unmodified and uninspected.
	//
	// Headers are copied onto w only once we've committed to writing this specific
	// response (immediately before each w.WriteHeader below), not unconditionally up
	// front: handleRequestHTTP's retry loop reuses the same w across attempts, so copying
	// headers before knowing whether this attempt aborts (the embedded-signal branch
	// below returns an error without ever calling WriteHeader) would leave them sitting
	// in w's header map, where a subsequent successful retry's headers would be added on
	// top of rather than replacing them - e.g. two Content-Length values in the response
	// actually sent to the client.
	chain, endpointID, endpointFound := s.findChainAndEndpointByURL(targetURL)
	if endpointFound && isJSONContentType(resp.Header.Get("Content-Type")) && providerNeedsEmbeddedRateLimitScan(s.providerForEndpoint(chain, endpointID)) {
		buffered, readErr := io.ReadAll(io.LimitReader(resp.Body, maxRateLimitScanBodyBytes+1))
		if readErr == nil && int64(len(buffered)) <= maxRateLimitScanBodyBytes {
			if sig := bodyCarriesRateLimitSignal(s.providerForEndpoint(chain, endpointID), buffered, resp.Header); sig.IsRateLimited {
				// The HTTP transaction itself succeeded (2xx) - don't mark the endpoint
				// unhealthy, just flag it as rate limited so selection avoids it, and
				// retry the same buffered request body against a different endpoint.
				s.handleRateLimit(chain, endpointID, "http", sig)
				log.Debug().Str("url", helpers.RedactAPIKey(targetURL)).Bool("daily_quota", sig.IsDailyQuota).Msg("2xx response carried an embedded rate-limit signal, retrying with a different endpoint")
				return fmt.Errorf("rate limited: embedded JSON-RPC rate-limit error in 2xx response")
			}
			copyResponseHeaders(w, resp.Header)
			w.WriteHeader(resp.StatusCode)
			_, err = w.Write(buffered)
			return err
		}
		// Exceeded the scan cap (or a read error occurred): stream the buffered prefix
		// plus whatever remains, unmodified and uninspected.
		copyResponseHeaders(w, resp.Header)
		w.WriteHeader(resp.StatusCode)
		_, err = io.Copy(w, io.MultiReader(bytes.NewReader(buffered), resp.Body))
		return err
	}

	// Set response status
	copyResponseHeaders(w, resp.Header)
	w.WriteHeader(resp.StatusCode)

	// Copy response body
	_, err = io.Copy(w, resp.Body)
	return err
}

// copyResponseHeaders copies resp's headers onto w, skipping CORS headers (already set
// by the CORS middleware). Callers must only invoke this once they've committed to
// writing this specific response - see the comment at defaultForwardRequestWithBodyFunc's
// call sites for why copying headers before that commitment is unsafe.
func copyResponseHeaders(w http.ResponseWriter, respHeader http.Header) {
	for key, values := range respHeader {
		if strings.HasPrefix(key, "Access-Control-") {
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
}

// maxRateLimitScanBodyBytes bounds how much of a 2xx response body is buffered to scan
// for an embedded JSON-RPC rate-limit error. Single-request rate-limit bodies are tiny;
// this cap exists so large legitimate responses aren't fully buffered in memory.
const maxRateLimitScanBodyBytes = 10 * 1024 * 1024

// isJSONContentType reports whether a Content-Type header value looks like JSON.
func isJSONContentType(contentType string) bool {
	return strings.Contains(contentType, "application/json") || strings.Contains(contentType, "text/json")
}

// providerForEndpoint looks up the configured provider name for a chain/endpoint, used to
// select provider-specific rate-limit detection behavior (see health.DetectRateLimit).
func (s *Server) providerForEndpoint(chain, endpointID string) string {
	chainEndpoints, ok := s.config.GetEndpointsForChain(chain)
	if !ok {
		return ""
	}
	return chainEndpoints[endpointID].Provider
}

// capacityWindowSeconds resolves the window width to track usage against for the WRITE
// path (recordCapacityUsage's usage counter). It must agree with whatever window
// effectiveCapacityCeiling's READ path is watching, or gating silently stops working -
// writes and reads would land in different Valkey bucket keys (see capacityBucketKey,
// which derives the bucket from windowSeconds itself). So: static Capacity's window if
// configured (unchanged, always stable); else the frozen window already recorded on a
// learned estimate, if one has been seeded (matching effectiveCapacityCeiling exactly);
// else the live-resolved adaptive-learning window as a bootstrap default before any
// estimate exists yet to freeze against.
func (s *Server) capacityWindowSeconds(chain, endpointID string, endpoint config.Endpoint) int {
	if endpoint.Capacity != nil {
		return endpoint.Capacity.WindowSeconds
	}
	if s.appConfig.CapacityLearningEnabled {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		estimate, err := s.valkeyClient.GetCapacityEstimate(ctx, chain, endpointID)
		cancel()
		if err == nil && estimate.HasEstimate {
			return estimate.WindowSeconds
		}
	}
	return config.ResolveCapacityLearning(endpoint.CapacityLearning).WindowSeconds
}

// recordCapacityUsage increments an endpoint's self-imposed capacity counter for the
// current window, on every dispatch attempt regardless of outcome - a failed/429'd
// attempt still spent a real unit of the provider's quota. No-op when capacity
// throttling is disabled entirely, or when the endpoint has no static Capacity and
// adaptive learning is also disabled.
func (s *Server) recordCapacityUsage(chain string, endpoint config.Endpoint, endpointID string) {
	if !s.appConfig.CapacityThrottlingEnabled {
		return
	}
	if endpoint.Capacity == nil && !s.appConfig.CapacityLearningEnabled {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := s.valkeyClient.IncrementCapacityCount(ctx, chain, endpointID, s.capacityWindowSeconds(chain, endpointID, endpoint)); err != nil {
		log.Debug().Err(err).Str("chain", chain).Str("endpoint", endpointID).Msg("Failed to record capacity usage")
	}
}

// applyLearnedCapacityDecrease is called from handleRateLimit on every confirmed
// rate-limit signal. It delegates to store.ApplyLearnedCapacityDecreaseIfEligible, the
// same shared implementation the standalone health checker calls, so the two processes -
// both mutating the same Valkey-persisted estimate - never diverge.
func (s *Server) applyLearnedCapacityDecrease(chain, endpointID string, endpoint config.Endpoint) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	store.ApplyLearnedCapacityDecreaseIfEligible(ctx, s.valkeyClient, chain, endpointID, endpoint, s.appConfig.CapacityThrottlingEnabled, s.appConfig.CapacityLearningEnabled)
}

// bodyCarriesRateLimitSignal reports whether a 2xx JSON-RPC response body (a single
// object or a batch array) contains an embedded rate-limit error - the case that makes
// Alchemy's HTTP-200-with-one-batch-item-429 blind spot visible, since HTTP status alone
// can't see it. Returns a zero-value signal if the body isn't a recognizable JSON-RPC
// response or carries no rate-limit error.
// providerNeedsEmbeddedRateLimitScan reports whether a provider is known to sometimes
// return a 2xx HTTP response with a rate-limit error embedded in the JSON-RPC body
// instead of a non-2xx status code. Currently only Alchemy, on batch requests - the
// specific quirk bodyCarriesRateLimitSignal exists to detect. Gating on this list keeps
// the buffer-and-scan cost (up to maxRateLimitScanBodyBytes held in memory, delaying
// time-to-first-byte) off every other provider's 2xx JSON responses.
func providerNeedsEmbeddedRateLimitScan(provider string) bool {
	return strings.EqualFold(provider, "alchemy")
}

func bodyCarriesRateLimitSignal(provider string, body []byte, headers http.Header) health.RateLimitSignal {
	var single health.RpcResponse
	if err := json.Unmarshal(body, &single); err == nil && single.Error != nil {
		return health.DetectRateLimit(provider, http.StatusOK, headers, &single)
	}

	var batch []health.RpcResponse
	if err := json.Unmarshal(body, &batch); err == nil {
		for i := range batch {
			if batch[i].Error == nil {
				continue
			}
			if sig := health.DetectRateLimit(provider, http.StatusOK, headers, &batch[i]); sig.IsRateLimited {
				return sig
			}
		}
	}

	return health.RateLimitSignal{}
}

// proxyWebSocketCopy copies messages from src to dst, forwarding close frames
// to the destination so both peers receive a proper WebSocket close handshake.
// It returns the first error and a bool indicating whether the error came from
// reading src (true) or writing to dst (false), so callers can attribute the
// failure to the correct peer.
// proxyWebSocketCopy copies messages from src to dst until an error occurs.
// closeSent, when non-nil, is set to true immediately before forwarding a close
// frame to dst. Callers use this to detect echoed close frames and avoid
// misattributing a backend-initiated close to the client.
func proxyWebSocketCopy(src, dst *websocket.Conn, closeSent *atomic.Bool) (error, bool) {
	for {
		msgType, msg, err := src.ReadMessage()
		if err != nil {
			if closeErr, ok := err.(*websocket.CloseError); ok {
				code := closeErr.Code
				// RFC 6455: 1005, 1006, 1015 must not be sent on the wire.
				switch code {
				case websocket.CloseNoStatusReceived,
					websocket.CloseAbnormalClosure,
					websocket.CloseTLSHandshake:
					code = websocket.CloseGoingAway
				}
				if closeSent != nil {
					closeSent.Store(true)
				}
				_ = dst.WriteMessage(websocket.CloseMessage,
					websocket.FormatCloseMessage(code, closeErr.Text))
			}
			return err, true // error came from reading src
		}
		if err := dst.WriteMessage(msgType, msg); err != nil {
			return err, false // error came from writing to dst
		}
	}
}

// isExpectedWSClose returns true for WebSocket close codes and errors that
// represent normal or benign disconnects and should NOT count as endpoint
// failures. This includes graceful closes, client-side disconnects, and
// EOF conditions at the TCP level.
func isExpectedWSClose(err error) bool {
	if err == nil {
		return false
	}
	if closeErr, ok := err.(*websocket.CloseError); ok {
		switch closeErr.Code {
		case websocket.CloseNormalClosure,
			websocket.CloseGoingAway,
			websocket.CloseNoStatusReceived,
			websocket.CloseAbnormalClosure:
			return true
		}
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		return true
	}
	return false
}

// defaultProxyWebSocket proxies a WebSocket connection between the client and the backend
func (s *Server) defaultProxyWebSocket(w http.ResponseWriter, r *http.Request, backendURL string) error {
	// IMPORTANT: Connect to backend FIRST before upgrading client connection.
	// This allows us to return HTTP error responses if the backend connection fails,
	// since the client connection hasn't been hijacked yet.
	backendConn, resp, err := websocket.DefaultDialer.Dial(backendURL, nil)
	if err != nil {
		chain, endpointID, found := s.findChainAndEndpointByURL(backendURL)

		// Check for non-2xx status codes during handshake
		if resp != nil && (resp.StatusCode < 200 || resp.StatusCode >= 300) {
			if sig := health.DetectRateLimit(s.providerForEndpoint(chain, endpointID), resp.StatusCode, resp.Header, nil); sig.IsRateLimited {
				// For a rate-limit signal (429, or Infura's 402 daily cap), mark unhealthy and return RateLimitError as signal
				if found {
					s.markEndpointUnhealthyProtocol(chain, endpointID, "ws")
				}
				log.Debug().Str("url", helpers.RedactAPIKey(backendURL)).Int("status_code", resp.StatusCode).Bool("daily_quota", sig.IsDailyQuota).Msg("WebSocket handshake rate limited")
				return &RateLimitError{
					StatusCode: resp.StatusCode,
					Message:    fmt.Sprintf("WebSocket handshake was rate-limited: HTTP %d", resp.StatusCode),
					Signal:     sig,
				}
			}

			// Special handling for 400 Bad Request - defer health decision to caller's retry logic.
			// The WebSocket handler caches the first 400 and retries with another endpoint. Only if the
			// second endpoint succeeds will it mark the first endpoint as unhealthy. This prevents
			// marking endpoints unhealthy due to client errors (bad requests) rather than endpoint failures.
			// We return early here to avoid any health marking, allowing the caller to make the decision.
			if resp.StatusCode == 400 {
				// Read response body for logging and passing through
				var respBodyBytes []byte
				if resp.Body != nil {
					respBodyBytes, _ = io.ReadAll(resp.Body)
				}
				log.Debug().Str("url", helpers.RedactAPIKey(backendURL)).Int("status_code", resp.StatusCode).Msg("WebSocket handshake returned 400 Bad Request")
				return &BadRequestError{
					StatusCode: resp.StatusCode,
					Message:    fmt.Sprintf("WebSocket handshake was rejected: HTTP %d", resp.StatusCode),
					Body:       respBodyBytes,
					Headers:    resp.Header,
				}
			}

			// Mark endpoint as unhealthy for any other non-2xx status code (skip 400)
			if found {
				s.markEndpointUnhealthyProtocol(chain, endpointID, "ws")
				log.Debug().Str("url", helpers.RedactAPIKey(backendURL)).Int("status_code", resp.StatusCode).Msg("WebSocket handshake returned non-2xx status, marked unhealthy")
			}
		} else if found {
			// Network/connection error - mark as unhealthy
			s.markEndpointUnhealthyProtocol(chain, endpointID, "ws")
		} else {
			log.Warn().Str("url", helpers.RedactAPIKey(backendURL)).Msg("Failed to find chain and endpoint for failed WS endpoint URL, cannot mark it as unhealthy.")
		}
		return err
	}
	defer backendConn.Close()

	// Only upgrade client connection after backend connection succeeds.
	// This ensures we can still send HTTP error responses if backend connection fails.
	var upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	clientConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Error().Err(err).Msg("WebSocket upgrade failed")
		// Close backend connection since we couldn't upgrade the client
		backendConn.Close()
		return err
	}
	// Proxy messages in both directions. Each goroutine reports (err, readErr)
	// via proxyWebSocketCopy, where readErr=true means the error came from
	// reading the source peer. Using separate channels lets us combine the
	// goroutine identity with readErr to attribute failures precisely:
	//
	//   backendErrc + readErr=true  → backend's ReadMessage failed  → backend at fault
	//   backendErrc + readErr=false → client's WriteMessage failed  → client at fault
	//   clientErrc  + readErr=true  → client's ReadMessage failed   → client at fault
	//   clientErrc  + readErr=false → backend's WriteMessage failed → backend at fault
	type wsResult struct {
		err     error
		readErr bool // true = error from src.ReadMessage; false = from dst.WriteMessage
	}
	// closeSentToClient is set to true by the backend-reading goroutine
	// immediately before it forwards a close frame to the client. If the
	// client-reading goroutine then fires with a read error, that close is
	// the client echoing our frame back — not a genuine client-initiated
	// close — so the backend remains at fault.
	var closeSentToClient atomic.Bool
	backendErrc := make(chan wsResult, 1)
	clientErrc := make(chan wsResult, 1)
	go func() {
		err, isRead := proxyWebSocketCopy(backendConn, clientConn, &closeSentToClient)
		backendErrc <- wsResult{err, isRead}
	}()
	go func() {
		err, isRead := proxyWebSocketCopy(clientConn, backendConn, nil)
		clientErrc <- wsResult{err, isRead}
	}()
	// Wait for the first side to stop, then close both connections so the
	// other goroutine unblocks and can be drained.
	var first wsResult
	var backendAtFault bool
	select {
	case first = <-backendErrc:
		backendAtFault = first.readErr // read from backend failed: backend at fault
		clientConn.Close()
		backendConn.Close()
		<-clientErrc
	case first = <-clientErrc:
		// If we already forwarded a close frame to the client, the client's
		// close is an echo of our frame, meaning the backend initiated the close.
		if first.readErr && closeSentToClient.Load() {
			backendAtFault = true
		} else {
			backendAtFault = !first.readErr // write to backend failed: backend at fault
		}
		clientConn.Close()
		backendConn.Close()
		<-backendErrc
	}

	if first.err != nil {
		if backendAtFault {
			if closeErr, ok := first.err.(*websocket.CloseError); ok {
				switch closeErr.Code {
				case websocket.CloseNormalClosure, // 1000: graceful, e.g. connection time limit
					websocket.CloseGoingAway,        // 1001: planned shutdown
					websocket.CloseNoStatusReceived: // 1005: no close code (ambiguous, treat as graceful)
					log.Debug().Err(first.err).Int("code", int(closeErr.Code)).
						Str("endpoint", helpers.RedactAPIKey(backendURL)).
						Msg("Backend closed WebSocket normally")
					return nil // graceful backend close is not a failure; caller marks success
				default:
					// 1006 (TCP drop), 1011 (internal error), and any other code
					// indicate the backend terminated unexpectedly.
					log.Debug().Err(first.err).Int("code", int(closeErr.Code)).
						Str("endpoint", helpers.RedactAPIKey(backendURL)).
						Msg("Backend closed WebSocket with unexpected code, counting as failure")
					if chain, endpointID, found := s.findChainAndEndpointByURL(backendURL); found {
						s.markEndpointUnhealthyProtocol(chain, endpointID, "ws")
					}
					return first.err // caller: isExpectedWSClose(1006)=true prevents retry
				}
			}
			// Non-close-frame error from backend (network error, read timeout, etc.)
			log.Debug().Err(first.err).
				Str("endpoint", helpers.RedactAPIKey(backendURL)).
				Msg("Backend WebSocket connection error, counting as failure")
			if chain, endpointID, found := s.findChainAndEndpointByURL(backendURL); found {
				s.markEndpointUnhealthyProtocol(chain, endpointID, "ws")
			}
			return first.err
		}
		// Client closed the connection. Any reason is fine, backend was healthy.
		log.Debug().Err(first.err).
			Str("endpoint", helpers.RedactAPIKey(backendURL)).
			Msg("Client closed WebSocket connection")
	}
	return nil
}

// GetRateLimitHandler returns the rate limit handler function for the health checker
func (s *Server) GetRateLimitHandler() func(chain, endpointID, protocol string, signal health.RateLimitSignal) {
	return s.handleRateLimit
}

// handleRateLimit handles rate limiting for an endpoint. The signal carries whatever
// recovery timing hint the provider gave (a Retry-After header, or Infura's daily-quota
// distinction), so the backoff can be seeded precisely instead of guessed.
func (s *Server) handleRateLimit(chain, endpointID, protocol string, signal health.RateLimitSignal) {
	log.Debug().Str("chain", chain).Str("endpoint", endpointID).Str("protocol", protocol).Msg("Handling rate limit")

	// Set the endpoint as rate limited in Valkey
	state, err := s.valkeyClient.GetRateLimitState(context.Background(), chain, endpointID)
	if err != nil {
		log.Error().Err(err).Str("chain", chain).Str("endpoint", endpointID).Msg("Failed to get rate limit state")
		return
	}

	// Mark as rate limited and initialize backoff state
	now := time.Now()
	state.RateLimited = true
	state.RecoveryAttempts = 0
	state.LastRecoveryCheck = now
	state.ConsecutiveSuccess = 0
	state.CurrentBackoff = s.initialBackoffForSignal(chain, endpointID, signal)

	// Set first rate limited time if this is the first time
	if state.FirstRateLimited.IsZero() {
		state.FirstRateLimited = now
	}

	if err := s.valkeyClient.SetRateLimitState(context.Background(), chain, endpointID, *state); err != nil {
		log.Error().Err(err).Str("chain", chain).Str("endpoint", endpointID).Msg("Failed to set rate limit state")
		return
	}

	log.Info().Str("chain", chain).Str("endpoint", endpointID).Str("protocol", protocol).Int("current_backoff", state.CurrentBackoff).Msg("Endpoint marked as rate limited")

	// Check for rate limits first (this signal), then approximate the endpoint's safe
	// throughput ceiling from it - only engages for endpoints with no static Capacity.
	if chainEndpoints, ok := s.config.GetEndpointsForChain(chain); ok {
		if ep, ok := chainEndpoints[endpointID]; ok {
			s.applyLearnedCapacityDecrease(chain, endpointID, ep)
		}
	}

	// Start rate limit recovery monitoring
	s.rateLimitScheduler.StartMonitoring(chain, endpointID)
}

// initialBackoffForSignal delegates to health.InitialBackoffForSignal, the same shared
// implementation the standalone health checker calls, so the two processes seed recovery
// backoff identically from the same signal.
func (s *Server) initialBackoffForSignal(chain, endpointID string, signal health.RateLimitSignal) int {
	return health.InitialBackoffForSignal(s.config, chain, endpointID, signal)
}
