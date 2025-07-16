package server

import (
	"context"
	"encoding/json"
	"io"
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

// setupRoutes configures the HTTP routes
func (s *Server) setupRoutes() {
	// Health check endpoint
	s.router.HandleFunc("/health", s.handleHealthCheck).Methods("GET")

	// Chain-specific endpoints
	for chain := range s.config.Endpoints {
		s.router.HandleFunc("/"+chain, s.handleChainRequest(chain)).Methods("POST")
		// Add GET handler for WebSocket upgrade
		s.router.HandleFunc("/"+chain, s.handleChainWebSocket(chain)).Methods("GET")
	}
}

// Start starts the HTTP server
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

// handleHealthCheck handles the health check endpoint
func (s *Server) handleHealthCheck(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "healthy"})
}

// handleChainRequest creates a handler for a specific chain
func (s *Server) handleChainRequest(chain string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Check if archive node is requested
		archive := r.URL.Query().Get("archive") == "true"

		// Get available endpoints
		endpoints := s.getAvailableEndpoints(chain, archive, false)
		if len(endpoints) == 0 {
			http.Error(w, "No available archive endpoints", http.StatusServiceUnavailable)
			return
		}

		// Select the best endpoint based on request counts
		endpoint := s.selectBestEndpoint(chain, endpoints)
		if endpoint == nil {
			http.Error(w, "No suitable endpoint found", http.StatusServiceUnavailable)
			return
		}

		// Forward the request
		if err := s.forwardRequest(w, r, endpoint.RPCURL); err != nil {
			log.Error().Err(err).Str("endpoint", endpoint.RPCURL).Msg("Failed to forward request")
			http.Error(w, "Failed to forward request", http.StatusBadGateway)
			return
		}

		// Increment request count
		if err := s.redisClient.IncrementRequestCount(r.Context(), chain, endpoint.Provider, "proxy_requests"); err != nil {
			log.Error().Err(err).Msg("Failed to increment request count")
		}
	}
}

// handleChainWebSocket creates a handler for WebSocket proxying for a specific chain
func (s *Server) handleChainWebSocket(chain string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Info().Str("path", r.URL.Path).Msg("Entered handleChainWebSocket")
		for k, v := range r.Header {
			log.Info().Str("header", k).Strs("values", v).Msg("Request header")
		}
		// Only handle WebSocket upgrade requests (case-insensitive, robust)
		if isWebSocketUpgrade(r) {
			archive := r.URL.Query().Get("archive") == "true"
			endpoints := s.getAvailableEndpoints(chain, archive, true)
			if len(endpoints) == 0 {
				http.Error(w, "No available archive endpoints", http.StatusServiceUnavailable)
				return
			}
			endpoint := s.selectBestEndpoint(chain, endpoints)
			if endpoint == nil || endpoint.WSURL == "" {
				http.Error(w, "No suitable WebSocket endpoint found", http.StatusServiceUnavailable)
				return
			}
			if err := s.proxyWebSocket(w, r, endpoint.WSURL); err != nil {
				// Check if this is a normal WebSocket closure
				if closeErr, ok := err.(*websocket.CloseError); ok {
					if closeErr.Code == websocket.CloseNormalClosure || closeErr.Code == websocket.CloseGoingAway {
						// Normal closure, just return without logging error
						log.Debug().
							Int("close_code", closeErr.Code).
							Str("close_text", closeErr.Text).
							Str("endpoint", helpers.RedactAPIKey(endpoint.WSURL)).
							Str("chain", chain).
							Msg("WebSocket connection closed normally")
						return
					}
				}
				log.Error().Err(err).Str("endpoint", endpoint.WSURL).Msg("Failed to proxy WebSocket")
				http.Error(w, "Failed to proxy WebSocket", http.StatusBadGateway)
				return
			}
			if err := s.redisClient.IncrementRequestCount(r.Context(), chain, endpoint.Provider, "proxy_requests"); err != nil {
				log.Error().Err(err).Msg("Failed to increment WebSocket request count")
			}
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

// containsToken checks if a comma-separated header contains a token (case-insensitive)
func containsToken(headerVal, token string) bool {
	for _, part := range splitAndTrim(headerVal) {
		if len(part) == len(token) && equalFold(part, token) {
			return true
		}
	}
	return false
}

func splitAndTrim(s string) []string {
	parts := strings.Split(s, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// getAvailableEndpoints returns available endpoints for a chain and protocol
func (s *Server) getAvailableEndpoints(chain string, archive bool, ws bool) []config.Endpoint {
	var endpoints []config.Endpoint

	// Try primary endpoints first
	primaryEndpoints := s.config.GetPrimaryEndpoints(chain)
	for _, endpoint := range primaryEndpoints {
		if !archive || (archive && endpoint.Type == "archive") {
			status, err := s.redisClient.GetEndpointStatus(context.Background(), chain, endpoint.Provider)
			if err == nil {
				if ws {
					if status.HasWS && status.HealthyWS {
						endpoints = append(endpoints, endpoint)
					}
				} else {
					if status.HasHTTP && status.HealthyHTTP {
						endpoints = append(endpoints, endpoint)
					}
				}
			}
		}
	}

	// If no primary endpoints are available, try fallback endpoints
	if len(endpoints) == 0 {
		fallbackEndpoints := s.config.GetFallbackEndpoints(chain)
		for _, endpoint := range fallbackEndpoints {
			if !archive || (archive && endpoint.Type == "archive") {
				status, err := s.redisClient.GetEndpointStatus(context.Background(), chain, endpoint.Provider)
				if err == nil {
					if ws {
						if status.HasWS && status.HealthyWS {
							endpoints = append(endpoints, endpoint)
						}
					} else {
						if status.HasHTTP && status.HealthyHTTP {
							endpoints = append(endpoints, endpoint)
						}
					}
				}
			}
		}
	}

	return endpoints
}

// selectBestEndpoint selects the best endpoint based on request counts
func (s *Server) selectBestEndpoint(chain string, endpoints []config.Endpoint) *config.Endpoint {
	if len(endpoints) == 0 {
		return nil
	}

	var bestEndpoint *config.Endpoint
	var minRequests int64 = -1

	for i := range endpoints {
		r24h, _, _, err := s.redisClient.GetCombinedRequestCounts(context.Background(), chain, endpoints[i].Provider)
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

// forwardRequest forwards the request to the target endpoint
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

// proxyWebSocket proxies a WebSocket connection between the client and the backend
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

	// Check if this is a normal WebSocket closure (close codes 1000 and 1001)
	if err != nil {
		if closeErr, ok := err.(*websocket.CloseError); ok {
			if closeErr.Code == websocket.CloseNormalClosure || closeErr.Code == websocket.CloseGoingAway {
				// This is a normal closure, not an error
				log.Debug().
					Int("close_code", closeErr.Code).
					Str("close_text", closeErr.Text).
					Str("endpoint", helpers.RedactAPIKey(backendURL)).
					Msg("WebSocket connection closed normally")
				return nil
			}
		}
	}

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
