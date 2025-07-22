package server

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"aetherlay/internal/config"
	"aetherlay/internal/helpers"
	"aetherlay/internal/store"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"github.com/rs/zerolog/log"
)

// Server represents the RPC load balancer server
type Server struct {
	config      *config.Config
	httpServer  *http.Server
	redisClient store.RedisClientIface
	router      *mux.Router

	forwardRequest func(w http.ResponseWriter, r *http.Request, targetURL string) error
	proxyWebSocket func(w http.ResponseWriter, r *http.Request, backendURL string) error
}

// NewServer creates a new server instance
func NewServer(cfg *config.Config, redisClient store.RedisClientIface) *Server {
	s := &Server{
		config:      cfg,
		redisClient: redisClient,
		router:      mux.NewRouter(),
	}

	s.forwardRequest = s.defaultForwardRequest
	s.proxyWebSocket = s.defaultProxyWebSocket

	s.setupRoutes()
	return s
}

// setupRoutes configures the HTTP routes for the server
func (s *Server) setupRoutes() {
	// Health check endpoint
	s.router.HandleFunc("/health", s.handleHealthCheck).Methods("GET")

	// Chain-specific endpoints
	for chain := range s.config.Endpoints {
		s.router.HandleFunc("/"+chain, s.handleRequestHTTP(chain)).Methods("POST")
		// Add GET handler for WebSocket upgrade
		s.router.HandleFunc("/"+chain, s.handleRequestWS(chain)).Methods("GET")
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return s.httpServer.Shutdown(ctx)
}

// handleHealthCheck handles the /health endpoint for health checks
func (s *Server) handleHealthCheck(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "healthy",
	})
}

// handleRequestHTTP creates a handler for HTTP requests for a specific chain
func (s *Server) handleRequestHTTP(chain string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Check if archive node is requested
		archive := r.URL.Query().Get("archive") == "true"

		// Get available endpoints
		endpoints := s.getAvailableEndpoints(chain, archive, false)
		if len(endpoints) == 0 {
			http.Error(w, "No available archive endpoints", http.StatusServiceUnavailable)
			return
		}

		// Try endpoints until one succeeds or we run out of endpoints
		var triedEndpoints []string
		for len(endpoints) > 0 {
			// Select the best endpoint based on request counts
			endpoint := s.selectBestEndpoint(chain, endpoints)
			if endpoint == nil {
				http.Error(w, "No suitable endpoint found", http.StatusServiceUnavailable)
				return
			}

			// Forward the request
			if err := s.forwardRequest(w, r, endpoint.Endpoint.HTTPURL); err != nil {
				log.Error().Err(err).Str("endpoint", endpoint.Endpoint.HTTPURL).Msg("Failed to forward request")
				triedEndpoints = append(triedEndpoints, endpoint.ID)

				// Remove the failed endpoint from the list and try again
				var remainingEndpoints []EndpointWithID
				for _, ep := range endpoints {
					if ep.ID != endpoint.ID {
						remainingEndpoints = append(remainingEndpoints, ep)
					}
				}
				endpoints = remainingEndpoints

				// If we still have endpoints to try, continue the loop
				if len(endpoints) > 0 {
					log.Info().Str("chain", chain).Str("failed_endpoint", endpoint.ID).Int("remaining_endpoints", len(endpoints)).Msg("Retrying with different endpoint")
					continue
				}
			} else {
				// We were able to send the request. Increment the request count and return.
				if err := s.redisClient.IncrementRequestCount(r.Context(), chain, endpoint.ID, "proxy_requests"); err != nil {
					log.Error().Err(err).Msg("Failed to increment request count")
				}
				return
			}
		}

		// If we get here, all endpoints failed
		log.Error().Str("chain", chain).Strs("tried_endpoints", triedEndpoints).Msg("All endpoints failed")
		http.Error(w, "Failed to forward request - all endpoints unavailable", http.StatusBadGateway)
	}
}

// handleRequestWS creates a handler for WebSocket requests for a specific chain
func (s *Server) handleRequestWS(chain string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Debug().Str("path", r.URL.Path).Msg("Entered handleRequestWS")
		for k, v := range r.Header {
			log.Debug().Str("header", k).Strs("values", v).Msg("Request header")
		}
		// Only handle WebSocket upgrade requests (case-insensitive, robust)
		if isWebSocketUpgrade(r) {
			archive := r.URL.Query().Get("archive") == "true"
			endpoints := s.getAvailableEndpoints(chain, archive, true)
			if len(endpoints) == 0 {
				http.Error(w, "No available archive endpoints", http.StatusServiceUnavailable)
				return
			}

			// Try endpoints until one succeeds or we run out of endpoints
			var triedEndpoints []string
			for len(endpoints) > 0 {
				endpoint := s.selectBestEndpoint(chain, endpoints)
				if endpoint == nil || endpoint.Endpoint.WSURL == "" {
					http.Error(w, "No suitable WebSocket endpoint found", http.StatusServiceUnavailable)
					return
				}

				if err := s.proxyWebSocket(w, r, endpoint.Endpoint.WSURL); err != nil {
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

					log.Error().Err(err).Str("endpoint", endpoint.Endpoint.WSURL).Msg("Failed to proxy WebSocket")
					triedEndpoints = append(triedEndpoints, endpoint.ID)

					// Remove the failed endpoint from the list and try again
					var remainingEndpoints []EndpointWithID
					for _, ep := range endpoints {
						if ep.ID != endpoint.ID {
							remainingEndpoints = append(remainingEndpoints, ep)
						}
					}
					endpoints = remainingEndpoints

					// If we still have endpoints to try, continue the loop
					if len(endpoints) > 0 {
						log.Info().Str("chain", chain).Str("failed_endpoint", endpoint.ID).Int("remaining_endpoints", len(endpoints)).Msg("Retrying WebSocket with different endpoint")
						continue
					}
				} else {
					// We were able to send the request. Increment the request count and return.
					if err := s.redisClient.IncrementRequestCount(r.Context(), chain, endpoint.ID, "proxy_requests"); err != nil {
						log.Error().Err(err).Msg("Failed to increment WebSocket request count")
					}
					return
				}
			}

			// If we get here, all endpoints failed
			log.Error().Str("chain", chain).Strs("tried_endpoints", triedEndpoints).Msg("All WebSocket endpoints failed")
			http.Error(w, "Failed to proxy WebSocket - all endpoints unavailable", http.StatusBadGateway)
			return
		}
		http.Error(w, "Not a WebSocket upgrade request", http.StatusBadRequest)
	}
}

// isWebSocketUpgrade checks if the request is a WebSocket upgrade (case-insensitive, robust)
func isWebSocketUpgrade(r *http.Request) bool {
	conn := r.Header.Get("Connection")
	upg := r.Header.Get("Upgrade")
	return containsToken(conn, "upgrade") && containsToken(upg, "websocket")
}

// containsToken checks if a comma-separated header contains a token (case-insensitive, exact match).
// It splits the header value, trims whitespace, and compares each token to the target using equalFold.
func containsToken(headerVal, token string) bool {
	for _, part := range splitAndTrim(headerVal) {
		if len(part) == len(token) && equalFold(part, token) {
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

// equalFold compares two ASCII strings for equality, ignoring case, without allocating new strings.
// It works by converting any uppercase ASCII letter to lowercase using arithmetic on their byte values.
// For example, 'B' (66) becomes 'b' (98) by adding 32 to it, which is the value of 'a' minus 'A'.
func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		// Convert uppercase ASCII to lowercase
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		// Compare 2 lowercase letters
		if ca != cb {
			return false
		}
	}
	return true
}

// EndpointWithID represents an endpoint along with its ID (map key)
type EndpointWithID struct {
	ID       string
	Endpoint config.Endpoint
}

// getAvailableEndpoints returns available endpoints for a chain and protocol
func (s *Server) getAvailableEndpoints(chain string, archive bool, ws bool) []EndpointWithID {
	var endpoints []EndpointWithID

	// Try primary endpoints first
	chainEndpoints, exists := s.config.GetEndpointsForChain(chain)
	if !exists {
		return endpoints
	}

	for endpointID, endpoint := range chainEndpoints {
		if endpoint.Role == "primary" {
			if !archive || (archive && endpoint.Type == "archive") {
				status, err := s.redisClient.GetEndpointStatus(context.Background(), chain, endpointID)
				if err == nil {
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
	}

	// If no primary endpoints are available, try fallback endpoints
	if len(endpoints) == 0 {
		for endpointID, endpoint := range chainEndpoints {
			if endpoint.Role == "fallback" {
				if !archive || (archive && endpoint.Type == "archive") {
					status, err := s.redisClient.GetEndpointStatus(context.Background(), chain, endpointID)
					if err == nil {
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
		}
	}

	return endpoints
}

// selectBestEndpoint selects the best endpoint based on request counts
func (s *Server) selectBestEndpoint(chain string, endpoints []EndpointWithID) *EndpointWithID {
	if len(endpoints) == 0 {
		return nil
	}

	var bestEndpoint *EndpointWithID
	var minRequests int64 = -1

	for i := range endpoints {
		r24h, _, _, err := s.redisClient.GetCombinedRequestCounts(context.Background(), chain, endpoints[i].ID)
		if err != nil {
			continue
		}

		if minRequests == -1 || r24h < minRequests {
			minRequests = r24h
			bestEndpoint = &endpoints[i]
		}
	}

	return bestEndpoint
}

// markEndpointUnhealthyProtocol marks the given endpoint as unhealthy for the specified protocol ("http" or "ws") in Redis.
func (s *Server) markEndpointUnhealthyProtocol(chain, endpointID, protocol string) {
	status, err := s.redisClient.GetEndpointStatus(context.Background(), chain, endpointID)
	if err != nil {
		log.Error().Err(err).Str("chain", chain).Str("endpoint", endpointID).Str("protocol", protocol).Msg("Failed to get endpoint status to mark unhealthy")
		return
	}
	switch protocol {
	case "http":
		status.HealthyHTTP = false
	case "ws":
		status.HealthyWS = false
	default:
		// Log a warning because this would be odd, so it would be nice to investigate how we got here
		log.Warn().Str("protocol", protocol).Msg("Unknown protocol, can't mark the endpoint as unhealthy")
		return
	}
	if err := s.redisClient.UpdateEndpointStatus(context.Background(), chain, endpointID, *status); err != nil {
		log.Error().Err(err).Str("chain", chain).Str("endpoint", endpointID).Str("protocol", protocol).Msg("Failed to update endpoint status to unhealthy")
	} else {
		log.Info().Str("chain", chain).Str("endpoint", endpointID).Str("protocol", protocol).Msg("Marked endpoint as unhealthy")
	}
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

// defaultForwardRequest forwards the request to the target endpoint
func (s *Server) defaultForwardRequest(w http.ResponseWriter, r *http.Request, targetURL string) error {
	// Create a new request
	req, err := http.NewRequest(r.Method, targetURL, r.Body)
	if err != nil {
		return err
	}

	// Copy headers
	for key, values := range r.Header {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}

	// Forward the request
	client := &http.Client{Timeout: 30 * time.Second}
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

	// Copy response headers
	for key, values := range resp.Header {
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
	backendConn, _, err := websocket.DefaultDialer.Dial(backendURL, nil)
	if err != nil {
		if chain, endpointID, found := s.findChainAndEndpointByURL(backendURL); found {
			s.markEndpointUnhealthyProtocol(chain, endpointID, "ws")
		} else {
			log.Warn().Str("url", backendURL).Msg("Failed to find chain and endpoint for failed WS endpoint URL, cannot mark it as unhealthy.")
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
