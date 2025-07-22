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

// Checker represents a health checker
type Checker struct {
	config      *config.Config
	redisClient store.RedisClientIface
	interval    time.Duration

	ephemeralChecks          map[string]*ephemeralState // key: chain|provider|protocol
	ephemeralChecksInterval  time.Duration
	ephemeralChecksThreshold int

	// For testability: allow patching health check methods
	CheckHTTPHealthFunc func(ctx context.Context, chain, provider string, endpoint config.Endpoint) bool
	CheckWSHealthFunc   func(ctx context.Context, chain, provider string, endpoint config.Endpoint) bool
}

// Add a map to track running ephemeral checks (at the top of the file, after Checker struct)
type ephemeralState struct {
	cancel context.CancelFunc
}

// NewChecker creates a new health checker
func NewChecker(cfg *config.Config, redisClient store.RedisClientIface, interval time.Duration, ephemeralChecksInterval time.Duration, ephemeralChecksThreshold int) *Checker {
	c := &Checker{
		config:                   cfg,
		redisClient:              redisClient,
		interval:                 interval,
		ephemeralChecks:          make(map[string]*ephemeralState),
		ephemeralChecksInterval:  ephemeralChecksInterval,
		ephemeralChecksThreshold: ephemeralChecksThreshold,
	}
	c.CheckHTTPHealthFunc = c.checkHTTPHealth
	c.CheckWSHealthFunc = c.checkWSHealth
	return c
}

// Start starts the health checker loop
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

// StartEphemeralChecks starts ephemeral health checks for unhealthy endpoints.
func (c *Checker) StartEphemeralChecks(ctx context.Context) {
	log.Info().Msg("Ephemeral check manager started")

	ticker := time.NewTicker(5 * time.Second) // How often to scan for unhealthy endpoints
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info().Msg("Ephemeral check manager shutting down")
			return
		case <-ticker.C:
			// Scan all endpoints for unhealthy status
			for chain, endpoints := range c.config.Endpoints {
				for provider, endpoint := range endpoints {
					status, err := c.redisClient.GetEndpointStatus(ctx, chain, provider)
					if err != nil {
						log.Error().Err(err).Str("chain", chain).Str("provider", provider).Msg("Failed to get endpoint status for ephemeral check")
						continue
					}
					// HTTP
					httpKey := chain + "|" + provider + "|http"
					if endpoint.HTTPURL != "" && !status.HealthyHTTP {
						if _, running := c.ephemeralChecks[httpKey]; !running {
							ctxEphemeral, cancel := context.WithCancel(ctx)
							c.ephemeralChecks[httpKey] = &ephemeralState{cancel: cancel}
							go c.runEphemeralCheckProtocol(ctxEphemeral, chain, provider, endpoint, c.ephemeralChecksInterval, c.ephemeralChecksThreshold, httpKey, "http")
							log.Info().Str("chain", chain).Str("provider", provider).Msg("Started ephemeral check for HTTP endpoint")
						}
					} else if status.HealthyHTTP {
						if state, running := c.ephemeralChecks[httpKey]; running {
							state.cancel()
							delete(c.ephemeralChecks, httpKey)
							log.Info().Str("chain", chain).Str("provider", provider).Msg("Stopped ephemeral check for HTTP endpoint (now healthy)")
						}
					}
					// WS
					wsKey := chain + "|" + provider + "|ws"
					if endpoint.WSURL != "" && !status.HealthyWS {
						if _, running := c.ephemeralChecks[wsKey]; !running {
							ctxEphemeral, cancel := context.WithCancel(ctx)
							c.ephemeralChecks[wsKey] = &ephemeralState{cancel: cancel}
							go c.runEphemeralCheckProtocol(ctxEphemeral, chain, provider, endpoint, c.ephemeralChecksInterval, c.ephemeralChecksThreshold, wsKey, "ws")
							log.Info().Str("chain", chain).Str("provider", provider).Msg("Started ephemeral check for WS endpoint")
						}
					} else if status.HealthyWS {
						if state, running := c.ephemeralChecks[wsKey]; running {
							state.cancel()
							delete(c.ephemeralChecks, wsKey)
							log.Info().Str("chain", chain).Str("provider", provider).Msg("Stopped ephemeral check for WS endpoint (now healthy)")
						}
					}
				}
			}
		}
	}
}

// runEphemeralCheckProtocol runs repeated health checks for a single protocol until healthy for threshold times
func (c *Checker) runEphemeralCheckProtocol(ctx context.Context, chain, provider string, endpoint config.Endpoint, interval time.Duration, threshold int, key string, protocol string) {
	consecutive := 0
	for {
		select {
		case <-ctx.Done():
			log.Debug().Str("chain", chain).Str("provider", provider).Str("protocol", protocol).Msg("Ephemeral check cancelled")
			return
		default:
			var healthy bool
			switch protocol {
			case "http":
				healthy = c.CheckHTTPHealthFunc(ctx, chain, provider, endpoint)
			case "ws":
				healthy = c.CheckWSHealthFunc(ctx, chain, provider, endpoint)
			default:
				log.Error().Str("chain", chain).Str("provider", provider).Str("protocol", protocol).Msg("Unknown protocol for ephemeral check")
				return
			}

			if healthy {
				consecutive++
				log.Debug().Str("chain", chain).Str("provider", provider).Str("protocol", protocol).Int("consecutive", consecutive).Msg("Ephemeral check: success")
				if consecutive >= threshold {
					log.Info().Str("chain", chain).Str("provider", provider).Str("protocol", protocol).Msg("Ephemeral check: protocol considered healthy again")
					// Mark protocol healthy in Redis
					status, err := c.redisClient.GetEndpointStatus(ctx, chain, provider)
					if err == nil {
						switch protocol {
						case "http":
							status.HealthyHTTP = true
						case "ws":
							status.HealthyWS = true
						}
						c.updateStatus(ctx, chain, provider, *status)
					}
					// Remove from ephemeralChecks
					if state, ok := c.ephemeralChecks[key]; ok {
						state.cancel()
						delete(c.ephemeralChecks, key)
					}
					return
				}
			} else {
				if consecutive > 0 {
					log.Debug().Str("chain", chain).Str("provider", provider).Str("protocol", protocol).Msg("Ephemeral check failed, resetting counter")
				}
				consecutive = 0
			}
			time.Sleep(interval)
		}
	}
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
		healthy := c.CheckHTTPHealthFunc(ctx, chain, provider, endpoint)
		httpResult <- healthy
	}()

	// Run WS health check in parallel
	go func() {
		healthy := c.CheckWSHealthFunc(ctx, chain, provider, endpoint)
		wsResult <- healthy
	}()

	// Collect results
	status.HasHTTP = endpoint.HTTPURL != ""
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
	if endpoint.HTTPURL == "" {
		return false
	}

	payload := []byte(`{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}`)
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint.HTTPURL, bytes.NewBuffer(payload))
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
			Str("endpoint", endpoint.HTTPURL).
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
