package server

import (
	"bytes"
	"context"
	"net/http"
	"time"

	"aetherlay/internal/config"
	"aetherlay/internal/store"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog/log"
)

// EphemeralHealthChecker represents a simple health checker for a specific endpoint
type EphemeralHealthChecker struct {
	config      *config.Config
	redisClient store.RedisClientIface
	interval    time.Duration
	chain       string
	provider    string
	endpoint    config.Endpoint
}

// RunOnce runs a single health check for the specific endpoint
func (e *EphemeralHealthChecker) RunOnce(ctx context.Context) {
	status := store.NewEndpointStatus()
	status.LastHealthCheck = time.Now()

	// Check HTTP health if endpoint has RPC URL
	if e.endpoint.RPCURL != "" {
		status.HasHTTP = true
		status.HealthyHTTP = e.checkHTTPHealth(ctx)
	}

	// Check WebSocket health if endpoint has WS URL
	if e.endpoint.WSURL != "" {
		status.HasWS = true
		status.HealthyWS = e.checkWSHealth(ctx)
	}

	// Update status in Redis
	if err := e.redisClient.UpdateEndpointStatus(ctx, e.chain, e.provider, status); err != nil {
		log.Error().Err(err).Str("chain", e.chain).Str("provider", e.provider).Msg("Failed to update endpoint status")
	} else {
		log.Info().
			Str("chain", e.chain).
			Str("provider", e.provider).
			Bool("healthy_http", status.HealthyHTTP).
			Bool("healthy_ws", status.HealthyWS).
			Msg("Updated endpoint status via ephemeral health check")
	}
}

// checkHTTPHealth performs HTTP health check for the specific endpoint
func (e *EphemeralHealthChecker) checkHTTPHealth(ctx context.Context) bool {
	payload := []byte(`{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}`)
	req, err := http.NewRequestWithContext(ctx, "POST", e.endpoint.RPCURL, bytes.NewBuffer(payload))
	if err != nil {
		return false
	}

	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	// Increment health request count
	if err := e.redisClient.IncrementRequestCount(ctx, e.chain, e.provider, "health_requests"); err != nil {
		log.Error().Err(err).Msg("Failed to increment health request count")
	}

	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// checkWSHealth performs WebSocket health check for the specific endpoint
func (e *EphemeralHealthChecker) checkWSHealth(ctx context.Context) bool {
	wsDialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	wsConn, _, err := wsDialer.Dial(e.endpoint.WSURL, nil)
	if err != nil {
		return false
	}
	defer wsConn.Close()

	// Increment health request count for WS
	if err := e.redisClient.IncrementRequestCount(ctx, e.chain, e.provider, "health_requests"); err != nil {
		log.Error().Err(err).Msg("Failed to increment WS health request count")
	}

	return true
}

// --- Server methods for ephemeral health checking ---

// SetEphemeralCheckInterval sets the interval for ephemeral health checks
func (s *Server) SetEphemeralCheckInterval(interval time.Duration) {
	s.healthCheckInterval = interval
}

// GetActiveEphemeralCheckers returns the number of active ephemeral health checkers
func (s *Server) GetActiveEphemeralCheckers() int {
	s.checkerMutex.RLock()
	defer s.checkerMutex.RUnlock()
	return len(s.ephemeralCheckers)
}

// markEndpointUnhealthy marks an endpoint as unhealthy and starts an ephemeral health checker
func (s *Server) markEndpointUnhealthy(chain, provider string, endpoint config.Endpoint) {
	// Mark endpoint as unhealthy
	status := store.NewEndpointStatus()
	status.HasHTTP = endpoint.RPCURL != ""
	status.HasWS = endpoint.WSURL != ""
	status.HealthyHTTP = false
	status.HealthyWS = false
	status.LastHealthCheck = time.Now()

	if err := s.redisClient.UpdateEndpointStatus(context.Background(), chain, provider, status); err != nil {
		log.Error().Err(err).Str("chain", chain).Str("provider", provider).Msg("Failed to mark endpoint as unhealthy")
		return
	}

	log.Info().Str("chain", chain).Str("provider", provider).Msg("Marked endpoint as unhealthy, starting ephemeral health checker")

	// Start ephemeral health checker
	s.startEphemeralHealthChecker(chain, provider, endpoint)
}

// startEphemeralHealthChecker starts a temporary health checker for a specific endpoint
func (s *Server) startEphemeralHealthChecker(chain, provider string, endpoint config.Endpoint) {
	checkerKey := chain + ":" + provider

	s.checkerMutex.Lock()
	// Check if a checker already exists for this endpoint
	if _, exists := s.ephemeralCheckers[checkerKey]; exists {
		s.checkerMutex.Unlock()
		log.Debug().Str("chain", chain).Str("provider", provider).Msg("Ephemeral health checker already running")
		return
	}

	// Create new ephemeral health checker for this specific endpoint
	checker := &EphemeralHealthChecker{
		config:      s.config,
		redisClient: s.redisClient,
		interval:    s.healthCheckInterval,
		chain:       chain,
		provider:    provider,
		endpoint:    endpoint,
	}
	s.ephemeralCheckers[checkerKey] = checker
	s.checkerMutex.Unlock()

	// Start the checker in a goroutine
	go func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		log.Info().Str("chain", chain).Str("provider", provider).Msg("Starting ephemeral health checker")
		
		// Run health checks until the endpoint becomes healthy
		ticker := time.NewTicker(s.healthCheckInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// Check if endpoint is healthy
				status, err := s.redisClient.GetEndpointStatus(ctx, chain, provider)
				if err != nil {
					log.Error().Err(err).Str("chain", chain).Str("provider", provider).Msg("Failed to get endpoint status")
					continue
				}

				// If endpoint is healthy, stop the ephemeral checker
				if (endpoint.RPCURL != "" && status.HealthyHTTP) || (endpoint.WSURL != "" && status.HealthyWS) {
					log.Info().Str("chain", chain).Str("provider", provider).Msg("Endpoint is healthy again, stopping ephemeral health checker")
					s.stopEphemeralHealthChecker(chain, provider)
					return
				}

				// Run health check
				checker.RunOnce(ctx)
			}
		}
	}()
}

// stopEphemeralHealthChecker stops and removes an ephemeral health checker
func (s *Server) stopEphemeralHealthChecker(chain, provider string) {
	checkerKey := chain + ":" + provider

	s.checkerMutex.Lock()
	defer s.checkerMutex.Unlock()

	if _, exists := s.ephemeralCheckers[checkerKey]; exists {
		delete(s.ephemeralCheckers, checkerKey)
		log.Debug().Str("chain", chain).Str("provider", provider).Msg("Stopped ephemeral health checker")
	}
} 
