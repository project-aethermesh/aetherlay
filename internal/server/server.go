package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"aetherlay/internal/cache"
	"aetherlay/internal/config"
	"aetherlay/internal/helpers"
	"aetherlay/internal/store"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"github.com/rs/zerolog/log"
)

// RateLimitError represents a rate limiting error during WebSocket handshake
type RateLimitError struct {
	StatusCode int
	Message    string
}

func (e *RateLimitError) Error() string {
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
	appConfig            *helpers.LoadedConfig
	config               *config.Config
	healthCache          *cache.HealthCache
	healthChecker        HealthCheckerIface
	httpServer           *http.Server
	maxRetries           int
	rateLimitScheduler   *RateLimitScheduler
	valkeyClient         store.ValkeyClientIface
	requestTimeout       time.Duration
	requestTimeoutPerTry time.Duration
	router               *mux.Router

	// Debouncing state tracking
	failureStates    map[string]*endpointFailureState
	failureThreshold int
	successThreshold int
	failureStatesMu  sync.RWMutex

	forwardRequestWithBody func(w http.ResponseWriter, ctx context.Context, method, targetURL string, bodyBytes []byte, headers http.Header) error
	proxyWebSocket         func(w http.ResponseWriter, r *http.Request, backendURL string) error
}

// NewServer creates a new server instance
func NewServer(cfg *config.Config, valkeyClient store.ValkeyClientIface, appConfig *helpers.LoadedConfig) *Server {
	s := &Server{
		appConfig:            appConfig,
		config:               cfg,
		failureStates:        make(map[string]*endpointFailureState),
		failureThreshold:     appConfig.EndpointFailureThreshold,
		healthCache:          cache.NewHealthCache(time.Duration(appConfig.HealthCacheTTL) * time.Second),
		maxRetries:           appConfig.ProxyMaxRetries,
		requestTimeout:       time.Duration(appConfig.ProxyTimeout) * time.Second,
		requestTimeoutPerTry: time.Duration(appConfig.ProxyTimeoutPerTry) * time.Second,
		router:               mux.NewRouter(),
		successThreshold:     appConfig.EndpointSuccessThreshold,
		valkeyClient:         valkeyClient,
	}

	s.forwardRequestWithBody = s.defaultForwardRequestWithBodyFunc
	s.proxyWebSocket = s.defaultProxyWebSocket

	// Initialize rate limit scheduler
	s.rateLimitScheduler = NewRateLimitScheduler(s.config, valkeyClient)

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
		Addr:    ":" + strconv.Itoa(port),
		Handler: s.router,
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
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "healthy",
	})
}

// handleReadinessCheck handles the /ready endpoint for readiness checks
// Returns 200 only when both LB and health-checker are ready
func (s *Server) handleReadinessCheck(w http.ResponseWriter, r *http.Request) {
	// Check LB's own readiness (Valkey connection)
	if err := s.valkeyClient.Ping(r.Context()); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status": "not_ready",
			"reason": "valkey_connection_failed",
		})
		return
	}

	// Check health-checker readiness
	if s.appConfig.StandaloneHealthChecks {
		// In standalone mode, check external health-checker service
		if !s.checkHealthCheckerServiceReady(r.Context()) {
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"status": "not_ready",
				"reason": "health_checker_service_not_ready",
			})
			return
		}
	} else {
		// In integrated mode, check integrated health checker
		if s.healthChecker != nil && !s.healthChecker.IsReady() {
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"status": "not_ready",
				"reason": "initial_health_check_in_progress",
			})
			return
		}
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "ready",
	})
}

// checkHealthCheckerServiceReady checks if the external health-checker service is ready
func (s *Server) checkHealthCheckerServiceReady(ctx context.Context) bool {
	// Create HTTP client with timeout
	client := &http.Client{
		Timeout: 2 * time.Second,
	}

	// Make request to health-checker's /ready endpoint
	url := strings.TrimSuffix(s.appConfig.HealthCheckerServiceURL, "/") + "/ready"
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		log.Debug().Err(err).Str("url", url).Msg("Failed to create request to health-checker service")
		return false
	}

	resp, err := client.Do(req)
	if err != nil {
		log.Debug().Err(err).Str("url", url).Msg("Failed to reach health-checker service")
		return false
	}
	defer resp.Body.Close()

	// Health-checker is ready if it returns 200
	return resp.StatusCode == http.StatusOK
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
				http.Error(w, "Failed to read request body", http.StatusBadRequest)
				return
			}
			r.Body.Close()
		}

		// Get all available endpoints with public-first logic
		allEndpoints := s.getAvailableEndpoints(chain, archive, false)

		log.Debug().Str("chain", chain).Bool("archive", archive).Int("available_endpoints", len(allEndpoints)).Msg("Retrieved available endpoints for HTTP request")

		if len(allEndpoints) == 0 {
			log.Debug().Str("chain", chain).Bool("archive", archive).Msg("No available endpoints found for HTTP request")
			http.Error(w, "No available endpoints", http.StatusServiceUnavailable)
			return
		}

		var triedEndpoints []string
		retryCount := 0
		publicAttemptCount := 0

		for retryCount < s.maxRetries && len(allEndpoints) > 0 {
			select {
			case <-ctx.Done():
				log.Error().Str("chain", chain).Msg("Request timeout reached")
				http.Error(w, "Request timeout", http.StatusGatewayTimeout)
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

			// Create a fresh request with a new body reader for each retry attempt
			err := s.forwardRequestWithBody(w, tryCtx, r.Method, endpoint.Endpoint.HTTPURL, bodyBytes, r.Header)
			tryCancel() // Always cancel the per-try context

			if err != nil {
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

				if len(allEndpoints) > 0 && retryCount < s.maxRetries {
					log.Debug().Str("chain", chain).Str("failed_endpoint", endpoint.ID).Int("public_attempt_count", publicAttemptCount).Int("remaining_endpoints", len(allEndpoints)).Int("retry", retryCount).Msg("Retrying HTTP request with different endpoint")
					continue
				}
			} else {
				// Success. Increment the request count and track success for debouncing.
				log.Debug().Str("chain", chain).Str("endpoint", endpoint.ID).Str("endpoint_url", helpers.RedactAPIKey(endpoint.Endpoint.HTTPURL)).Int("retry", retryCount).Msg("HTTP request succeeded")
				if err := s.valkeyClient.IncrementRequestCount(ctx, chain, endpoint.ID, "proxy_requests"); err != nil {
					log.Error().Err(err).Str("endpoint", endpoint.ID).Msg("Failed to increment request count")
				}
				// Track success for health debouncing
				s.markEndpointHealthyAttempt(chain, endpoint.ID, "http")
				return
			}
		}

		// If we get here, all retries failed
		if retryCount >= s.maxRetries {
			log.Error().Str("chain", chain).Strs("tried_endpoints", triedEndpoints).Int("max_retries", s.maxRetries).Msg("Max retries reached")
			http.Error(w, "Max retries reached, all endpoints unavailable", http.StatusBadGateway)
		} else {
			log.Error().Str("chain", chain).Strs("tried_endpoints", triedEndpoints).Msg("All endpoints failed")
			http.Error(w, "Failed to forward request, all endpoints unavailable", http.StatusBadGateway)
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
				http.Error(w, "No available WebSocket endpoints", http.StatusServiceUnavailable)
				return
			}

			var triedEndpoints []string
			retryCount := 0
			publicAttemptCount := 0

			for retryCount < s.maxRetries && len(allEndpoints) > 0 {
				select {
				case <-ctx.Done():
					log.Error().Str("chain", chain).Msg("WebSocket request timeout reached")
					http.Error(w, "WebSocket request timeout", http.StatusGatewayTimeout)
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

				err := s.proxyWebSocket(w, reqWithCtx, endpoint.Endpoint.WSURL)
				tryCancel() // Always cancel the per-try context

				if err != nil {
					// Check if this is a 429 rate limiting error during handshake
					if _, ok := err.(*RateLimitError); ok {
						log.Debug().Str("chain", chain).Str("endpoint", endpoint.ID).Int("retry", retryCount).Msg("WebSocket handshake rate limited")
						s.handleRateLimit(chain, endpoint.ID, "ws")
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
								http.Error(w, "WebSocket max retries reached after rate limits, all endpoints unavailable", http.StatusBadGateway)
							} else {
								log.Error().Str("chain", chain).Strs("tried_endpoints", triedEndpoints).Msg("All WebSocket endpoints rate limited")
								http.Error(w, "All WebSocket endpoints rate limited", http.StatusTooManyRequests)
							}
							return
						}
					}
					// Check if this is a normal WebSocket closure
					if closeErr, ok := err.(*websocket.CloseError); ok {
						if closeErr.Code == websocket.CloseNormalClosure || closeErr.Code == websocket.CloseGoingAway {
							// Normal closure
							log.Debug().
								Int("close_code", closeErr.Code).
								Str("close_text", closeErr.Text).
								Str("endpoint", helpers.RedactAPIKey(endpoint.Endpoint.WSURL)).
								Str("chain", chain).
								Msg("WebSocket connection closed normally")
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
					// Success. Increment the request count and track success for debouncing.
					log.Debug().Str("chain", chain).Str("endpoint", endpoint.ID).Str("endpoint_url", helpers.RedactAPIKey(endpoint.Endpoint.WSURL)).Int("retry", retryCount).Msg("WebSocket connection succeeded")
					if err := s.valkeyClient.IncrementRequestCount(ctx, chain, endpoint.ID, "proxy_requests"); err != nil {
						log.Error().Err(err).Str("endpoint", endpoint.ID).Msg("Failed to increment WebSocket request count")
					}
					// Track success for health debouncing
					s.markEndpointHealthyAttempt(chain, endpoint.ID, "ws")
					return
				}
			}

			// If we get here, all retries failed
			if retryCount >= s.maxRetries {
				log.Error().Str("chain", chain).Strs("tried_endpoints", triedEndpoints).Int("max_retries", s.maxRetries).Msg("WebSocket max retries reached")
				http.Error(w, "WebSocket max retries reached, all endpoints unavailable", http.StatusBadGateway)
			} else {
				log.Error().Str("chain", chain).Strs("tried_endpoints", triedEndpoints).Msg("All WebSocket endpoints failed")
				http.Error(w, "Failed to proxy WebSocket, all endpoints unavailable", http.StatusBadGateway)
			}
			return
		}
		http.Error(w, "GET requests to this endpoint are only supported for WebSocket upgrade requests. Otherwise, please use POST.", http.StatusBadRequest)
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

// selectBestEndpointByRole selects the best endpoint of a specific role based on request counts
func (s *Server) selectBestEndpointByRole(chain string, endpoints []EndpointWithID, role string) *EndpointWithID {
	var bestEndpoint *EndpointWithID
	var minRequests int64 = -1

	for i := range endpoints {
		// Skip endpoints that don't match the requested role
		if endpoints[i].Endpoint.Role != role {
			continue
		}

		r24h, _, _, err := s.valkeyClient.GetCombinedRequestCounts(context.Background(), chain, endpoints[i].ID)
		// Skip endpoints where we can't get request count data
		if err != nil {
			continue
		}

		// Select endpoint with lowest 24h request count (or first one if minRequests is uninitialized)
		if minRequests == -1 || r24h < minRequests {
			minRequests = r24h
			bestEndpoint = &endpoints[i]
		}
	}

	return bestEndpoint
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

	// Check for HTTP status codes that should trigger retries
	if s.shouldRetry(resp.StatusCode) {
		if chain, endpointID, found := s.findChainAndEndpointByURL(targetURL); found {
			if resp.StatusCode == 429 {
				// For 429 (Too Many Requests), use the rate limit handler
				s.markEndpointUnhealthyProtocol(chain, endpointID, "http")
				s.handleRateLimit(chain, endpointID, "http")
				log.Debug().Str("url", helpers.RedactAPIKey(targetURL)).Int("status_code", resp.StatusCode).Msg("Endpoint returned 429 (Too Many Requests), handling rate limit")
			} else {
				// For 5xx errors, mark as unhealthy
				s.markEndpointUnhealthyProtocol(chain, endpointID, "http")
				log.Debug().Str("url", helpers.RedactAPIKey(targetURL)).Int("status_code", resp.StatusCode).Msg("Endpoint returned server error, marked unhealthy")
			}
		}
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, resp.Status)
	}

	// Copy response headers, but skip CORS headers since we set our own
	for key, values := range resp.Header {
		// Skip CORS headers to avoid duplication
		if strings.HasPrefix(key, "Access-Control-") {
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}

	// Set response status
	w.WriteHeader(resp.StatusCode)

	// Copy response body
	_, err = io.Copy(w, resp.Body)
	return err
}

// shouldRetry returns true if the HTTP status code should trigger a retry
func (s *Server) shouldRetry(statusCode int) bool {
	// Retry on 5xx server errors and 429 Too Many Requests
	return (statusCode >= 500 && statusCode < 600) || statusCode == 429
}

// proxyWebSocketCopy copies messages from src to dst
func proxyWebSocketCopy(src, dst *websocket.Conn) error {
	for {
		msgType, msg, err := src.ReadMessage()
		if err != nil {
			return err
		}
		if err := dst.WriteMessage(msgType, msg); err != nil {
			return err
		}
	}
}

// defaultProxyWebSocket proxies a WebSocket connection between the client and the backend
func (s *Server) defaultProxyWebSocket(w http.ResponseWriter, r *http.Request, backendURL string) error {
	// Upgrade the incoming request to a WebSocket
	var upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	clientConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Error().Err(err).Msg("WebSocket upgrade failed")
		return err
	}
	defer clientConn.Close()

	// Connect to the backend WebSocket
	backendConn, resp, err := websocket.DefaultDialer.Dial(backendURL, nil)
	if err != nil {
		// Check if this is a 429 rate limit response during handshake
		if resp != nil && resp.StatusCode == 429 {
			log.Debug().Str("url", helpers.RedactAPIKey(backendURL)).Int("status_code", resp.StatusCode).Msg("WebSocket handshake rate limited")
			return &RateLimitError{
				StatusCode: resp.StatusCode,
				Message:    fmt.Sprintf("WebSocket handshake was rate-limited: HTTP %d", resp.StatusCode),
			}
		}

		if chain, endpointID, found := s.findChainAndEndpointByURL(backendURL); found {
			s.markEndpointUnhealthyProtocol(chain, endpointID, "ws")
		} else {
			log.Warn().Str("url", helpers.RedactAPIKey(backendURL)).Msg("Failed to find chain and endpoint for failed WS endpoint URL, cannot mark it as unhealthy.")
		}
		return err
	}
	defer backendConn.Close()

	// Proxy messages in both directions
	errc := make(chan error, 2)
	go func() {
		err := proxyWebSocketCopy(clientConn, backendConn)
		errc <- err
	}()
	go func() {
		err := proxyWebSocketCopy(backendConn, clientConn)
		errc <- err
	}()
	// Wait for one direction to fail/close
	err = <-errc

	// Mark endpoint as unhealthy for WS if error is not a normal closure
	if err != nil {
		if closeErr, ok := err.(*websocket.CloseError); ok {
			if closeErr.Code == websocket.CloseNormalClosure || closeErr.Code == websocket.CloseGoingAway {
				log.Debug().Int("close_code", closeErr.Code).Str("close_text", closeErr.Text).Str("endpoint", helpers.RedactAPIKey(backendURL)).Msg("WebSocket connection closed normally")
				return nil
			}
		}
		// Do not mark as unhealthy for timeouts
		if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			log.Debug().Err(err).Str("endpoint", helpers.RedactAPIKey(backendURL)).Msg("WebSocket timeout, not marking endpoint as unhealthy")
			return err
		}
		if chain, endpointID, found := s.findChainAndEndpointByURL(backendURL); found {
			s.markEndpointUnhealthyProtocol(chain, endpointID, "ws")
		}
	}

	return err
}

// GetRateLimitHandler returns the rate limit handler function for the health checker
func (s *Server) GetRateLimitHandler() func(chain, endpointID, protocol string) {
	return s.handleRateLimit
}

// handleRateLimit handles rate limiting for an endpoint
func (s *Server) handleRateLimit(chain, endpointID, protocol string) {
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
	state.CurrentBackoff = 0 // Will be set to initial backoff on first attempt

	// Set first rate limited time if this is the first time
	if state.FirstRateLimited.IsZero() {
		state.FirstRateLimited = now
	}

	if err := s.valkeyClient.SetRateLimitState(context.Background(), chain, endpointID, *state); err != nil {
		log.Error().Err(err).Str("chain", chain).Str("endpoint", endpointID).Msg("Failed to set rate limit state")
		return
	}

	log.Info().Str("chain", chain).Str("endpoint", endpointID).Str("protocol", protocol).Msg("Endpoint marked as rate limited")

	// Start rate limit recovery monitoring
	s.rateLimitScheduler.StartMonitoring(chain, endpointID)
}
