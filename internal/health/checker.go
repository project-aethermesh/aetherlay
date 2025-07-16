package health

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"time"

	"aetherlay/internal/config"
	"aetherlay/internal/store"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog/log"
)

// Checker represents the health checker
type Checker struct {
	config      *config.Config
	redisClient *store.RedisClient
	interval    time.Duration
}

// NewChecker creates a new health checker
func NewChecker(cfg *config.Config, redisClient *store.RedisClient, interval time.Duration) *Checker {
	return &Checker{
		config:      cfg,
		redisClient: redisClient,
		interval:    interval,
	}
}

// Start starts the health checker
func (c *Checker) Start(ctx context.Context) {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	// Run initial health check
	c.checkAllEndpoints(ctx)

	for {
		select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				c.checkAllEndpoints(ctx)
		}
	}
}

// RunOnce runs the health check a single time
func (c *Checker) RunOnce(ctx context.Context) {
	c.checkAllEndpoints(ctx)
}

// checkAllEndpoints checks the health of all endpoints
func (c *Checker) checkAllEndpoints(ctx context.Context) {
	for chain, endpoints := range c.config.Endpoints {
		for provider, endpoint := range endpoints {
			go c.checkEndpoint(ctx, chain, provider, endpoint)
		}
	}
}

// checkEndpoint checks the health of a single endpoint
func (c *Checker) checkEndpoint(ctx context.Context, chain, provider string, endpoint config.Endpoint) {
	status := store.NewEndpointStatus()
	status.LastHealthCheck = time.Now()

	// Create channels to collect results from parallel health checks
	httpResult := make(chan bool, 1)
	wsResult := make(chan bool, 1)

	// Run HTTP health check in parallel
	go func() {
		healthy := c.checkHTTPHealth(ctx, chain, provider, endpoint)
		httpResult <- healthy
	}()

	// Run WS health check in parallel
	go func() {
		healthy := c.checkWSHealth(ctx, chain, provider, endpoint)
		wsResult <- healthy
	}()

	// Collect results
	status.HasHTTP = endpoint.RPCURL != ""
	status.HasWS = endpoint.WSURL != ""
	status.HealthyHTTP = <-httpResult
	status.HealthyWS = <-wsResult

	// Get current request counts
	r24h, r1m, rAll, err := c.redisClient.GetCombinedRequestCounts(ctx, chain, provider)
	if err == nil {
		status.Requests24h = r24h
		status.Requests1Month = r1m
		status.RequestsLifetime = rAll
	}

	// Update status in Redis
	c.updateStatus(ctx, chain, provider, status)
}

// checkHTTPHealth performs HTTP health check and returns health status
func (c *Checker) checkHTTPHealth(ctx context.Context, chain, provider string, endpoint config.Endpoint) bool {
	if endpoint.RPCURL == "" {
		return false
	}

	payload := []byte(`{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}`)
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint.RPCURL, bytes.NewBuffer(payload))
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
	if err := c.redisClient.IncrementRequestCount(ctx, chain, provider, "health_requests"); err != nil {
		log.Error().Err(err).Msg("Failed to increment health request count")
	}

	healthy := resp.StatusCode >= 200 && resp.StatusCode < 300
	if !healthy {
		// Read and log up to 512 bytes of the response body for debugging
		bodyBytes := make([]byte, 512)
		n, _ := io.ReadFull(resp.Body, bodyBytes)
		log.Error().
			Str("endpoint", endpoint.RPCURL).
			Str("chain", chain).
			Str("provider", provider).
			Int("status_code", resp.StatusCode).
			Str("body", string(bodyBytes[:n])).
			Msg("Health check failed: endpoint returned non-2xx status")
	}

	return healthy
}

// checkWSHealth performs WebSocket health check and returns health status
func (c *Checker) checkWSHealth(ctx context.Context, chain, provider string, endpoint config.Endpoint) bool {
	if endpoint.WSURL == "" {
		return false
	}

	wsDialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	wsConn, _, err := wsDialer.Dial(endpoint.WSURL, nil)
	if err != nil {
		log.Error().
			Err(err).
			Str("endpoint", endpoint.WSURL).
			Str("chain", chain).
			Str("provider", provider).
			Msg("WebSocket health check failed: failed to establish connection")
		return false
	}

	wsConn.Close()

	// Increment health request count for WS
	if err := c.redisClient.IncrementRequestCount(ctx, chain, provider, "health_requests"); err != nil {
		log.Error().Err(err).Msg("Failed to increment WS health request count")
	}

	return true
}

// updateStatus updates the endpoint status in Redis
func (c *Checker) updateStatus(ctx context.Context, chain, provider string, status store.EndpointStatus) {
	if err := c.redisClient.UpdateEndpointStatus(ctx, chain, provider, status); err != nil {
		log.Error().Err(err).
			Str("chain", chain).
			Str("provider", provider).
			Msg("Failed to update endpoint status")
	} else {
		log.Info().
			Str("chain", chain).
			Str("provider", provider).
			Bool("healthy_http", status.HealthyHTTP).
			Bool("healthy_ws", status.HealthyWS).
			Bool("has_http", status.HasHTTP).
			Bool("has_ws", status.HasWS).
			Msg("Updated endpoint status")
	}
}
