package health

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"aetherlay/internal/config"
	"aetherlay/internal/helpers"
	"aetherlay/internal/metrics"
	"aetherlay/internal/store"

	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog/log"
)

// ErrMethodNotFound indicates that the RPC method is not supported by the endpoint
var ErrMethodNotFound = errors.New("method not found")

// rpcResponse represents a JSON-RPC 2.0 response
type rpcResponse struct {
	Result any `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// checkRPCError checks for errors in an RPC response and handles method-not-found errors specially
func checkRPCError(response *rpcResponse, method, protocol, chain, endpointID, url string) error {
	if response.Error == nil {
		return nil
	}

	// Check for "method not found" errors
	methodNotFound := response.Error.Code == -32601 || containsMethodNotFound(response.Error.Message)

	if methodNotFound && method == "eth_syncing" {
		log.Debug().
			Str("chain", chain).
			Str("endpoint", helpers.RedactAPIKey(url)).
			Str("endpoint_id", endpointID).
			Int("error_code", response.Error.Code).
			Str("error_message", response.Error.Message).
			Str("method", method).
			Msg("eth_syncing not supported by the endpoint, assuming it is fully synced")
		return ErrMethodNotFound
	}

	log.Error().
		Str("chain", chain).
		Str("endpoint", helpers.RedactAPIKey(url)).
		Str("endpoint_id", endpointID).
		Int("error_code", response.Error.Code).
		Str("error_message", response.Error.Message).
		Str("method", method).
		Msgf("%s RPC call failed: JSON-RPC error response", protocol)

	return errors.New(response.Error.Message)
}

// containsMethodNotFound checks if an error message indicates a method is not supported
func containsMethodNotFound(message string) bool {
	message = strings.ToLower(message)
	return strings.Contains(message, "method not found") ||
		strings.Contains(message, "method does not exist") ||
		strings.Contains(message, "is not available") ||
		strings.Contains(message, "not supported") ||
		strings.Contains(message, "unknown method")
}

// Checker represents a health checker
type Checker struct {
	config                *config.Config
	concurrency           int
	ephemeralChecksEnabled bool
	healthCheckSyncStatus bool
	interval              time.Duration
	valkeyClient          store.ValkeyClientIface

	ephemeralChecks          map[string]*ephemeralState // key: chain|endpointID|protocol
	ephemeralChecksInterval  time.Duration
	ephemeralChecksThreshold int

	// Rate limit handler function provided by server
	HandleRateLimitFunc func(chain, endpointID, protocol string)

	// For testability: allow patching health check methods
	CheckHTTPHealthFunc func(ctx context.Context, chain, endpointID string, endpoint config.Endpoint) bool
	CheckWSHealthFunc   func(ctx context.Context, chain, endpointID string, endpoint config.Endpoint) bool

	// Startup synchronization
	initialCheckDone sync.WaitGroup
	isReady          bool
	readyMu          sync.RWMutex
}

// Add a map to track running ephemeral checks (at the top of the file, after Checker struct)
type ephemeralState struct {
	cancel context.CancelFunc
}

// NewChecker creates a new health checker
func NewChecker(cfg *config.Config, valkeyClient store.ValkeyClientIface, interval time.Duration, ephemeralChecksInterval time.Duration, ephemeralChecksThreshold int, healthCheckSyncStatus bool, concurrency int, ephemeralChecksEnabled bool) *Checker {
	c := &Checker{
		config:                   cfg,
		concurrency:              concurrency,
		ephemeralChecksEnabled:   ephemeralChecksEnabled,
		healthCheckSyncStatus:    healthCheckSyncStatus,
		interval:                 interval,
		valkeyClient:             valkeyClient,
		ephemeralChecks:          make(map[string]*ephemeralState),
		ephemeralChecksInterval:  ephemeralChecksInterval,
		ephemeralChecksThreshold: ephemeralChecksThreshold,
		isReady:                  false,
	}
	c.CheckHTTPHealthFunc = c.checkHTTPHealth
	c.CheckWSHealthFunc = c.checkWSHealth

	// Initialize WaitGroup with 1 to block until initial check completes
	c.initialCheckDone.Add(1)

	return c
}

// IsReady returns true if the initial health check has completed
func (c *Checker) IsReady() bool {
	c.readyMu.RLock()
	defer c.readyMu.RUnlock()
	return c.isReady
}

// WaitForInitialCheck blocks until the initial health check completes
func (c *Checker) WaitForInitialCheck() {
	c.initialCheckDone.Wait()
}

// Start starts the health checker loop
func (c *Checker) Start(ctx context.Context) {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	// Run initial health check and wait for completion
	c.checkAllEndpointsAndWait(ctx, true)

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
	if !c.ephemeralChecksEnabled {
		log.Info().Msg("Ephemeral checks disabled (EPHEMERAL_CHECKS_ENABLED=false), skipping ephemeral check manager")
		return
	}

	log.Info().Msg("Ephemeral check manager started")

	// Run a one-time health check for all endpoints at startup
	for chain, endpoints := range c.config.Endpoints {
		for endpointID, endpoint := range endpoints {
			if endpoint.HTTPURL != "" {
				_ = c.CheckHTTPHealthFunc(ctx, chain, endpointID, endpoint)
			}
			if endpoint.WSURL != "" {
				_ = c.CheckWSHealthFunc(ctx, chain, endpointID, endpoint)
			}
		}
	}

	ticker := time.NewTicker(c.ephemeralChecksInterval) // How often to scan for unhealthy endpoints
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info().Msg("Ephemeral check manager shutting down")
			return
		case <-ticker.C:
			// Scan all endpoints for unhealthy status
			for chain, endpoints := range c.config.Endpoints {
				for endpointID, endpoint := range endpoints {
					status, err := c.valkeyClient.GetEndpointStatus(ctx, chain, endpointID)
					if err != nil {
						log.Error().Err(err).Str("chain", chain).Str("endpoint_id", endpointID).Msg("Failed to get endpoint status for ephemeral check")
						continue
					}
					// Check if endpoint is rate limited
					rateLimitState, err := c.valkeyClient.GetRateLimitState(ctx, chain, endpointID)
					if err == nil && rateLimitState.RateLimited {
						log.Debug().Str("chain", chain).Str("endpoint_id", endpointID).Msg("Skipping ephemeral checks for rate-limited endpoint")
						continue
					}

					// HTTP
					httpKey := chain + "|" + endpointID + "|http"
					if endpoint.HTTPURL != "" && !status.HealthyHTTP {
						if _, running := c.ephemeralChecks[httpKey]; !running {
							ctxEphemeral, cancel := context.WithCancel(ctx)
							c.ephemeralChecks[httpKey] = &ephemeralState{cancel: cancel}
							go c.runEphemeralCheckProtocol(ctxEphemeral, chain, endpointID, endpoint, c.ephemeralChecksInterval, c.ephemeralChecksThreshold, httpKey, "http")
							log.Info().Str("chain", chain).Str("endpoint_id", endpointID).Msg("Started ephemeral check for HTTP endpoint")
						}
					} else if status.HealthyHTTP {
						if state, running := c.ephemeralChecks[httpKey]; running {
							state.cancel()
							delete(c.ephemeralChecks, httpKey)
							log.Info().Str("chain", chain).Str("endpoint_id", endpointID).Msg("Stopped ephemeral check for HTTP endpoint (now healthy)")
						}
					}
					// WS
					wsKey := chain + "|" + endpointID + "|ws"
					if endpoint.WSURL != "" && !status.HealthyWS {
						if _, running := c.ephemeralChecks[wsKey]; !running {
							ctxEphemeral, cancel := context.WithCancel(ctx)
							c.ephemeralChecks[wsKey] = &ephemeralState{cancel: cancel}
							go c.runEphemeralCheckProtocol(ctxEphemeral, chain, endpointID, endpoint, c.ephemeralChecksInterval, c.ephemeralChecksThreshold, wsKey, "ws")
							log.Info().Str("chain", chain).Str("endpoint_id", endpointID).Msg("Started ephemeral check for WS endpoint")
						}
					} else if status.HealthyWS {
						if state, running := c.ephemeralChecks[wsKey]; running {
							state.cancel()
							delete(c.ephemeralChecks, wsKey)
							log.Info().Str("chain", chain).Str("endpoint_id", endpointID).Msg("Stopped ephemeral check for WS endpoint (now healthy)")
						}
					}
				}
			}
		}
	}
}

// runEphemeralCheckProtocol runs repeated health checks for a single protocol until healthy for threshold times
func (c *Checker) runEphemeralCheckProtocol(ctx context.Context, chain, endpointID string, endpoint config.Endpoint, interval time.Duration, threshold int, key string, protocol string) {
	consecutive := 0
	for {
		select {
		case <-ctx.Done():
			log.Debug().Str("chain", chain).Str("endpoint_id", endpointID).Str("protocol", protocol).Msg("Ephemeral check cancelled")
			return
		default:
			var healthy bool
			switch protocol {
			case "http":
				healthy = c.CheckHTTPHealthFunc(ctx, chain, endpointID, endpoint)
			case "ws":
				healthy = c.CheckWSHealthFunc(ctx, chain, endpointID, endpoint)
			default:
				log.Error().Str("chain", chain).Str("endpoint_id", endpointID).Str("protocol", protocol).Msg("Unknown protocol for ephemeral check")
				return
			}

			if healthy {
				consecutive++
				log.Debug().Str("chain", chain).Str("endpoint_id", endpointID).Str("protocol", protocol).Int("consecutive", consecutive).Msg("Ephemeral check: success")
				if consecutive >= threshold {
					log.Info().Str("chain", chain).Str("endpoint_id", endpointID).Str("protocol", protocol).Msg("Ephemeral check: protocol considered healthy again")
					// Mark protocol healthy in Valkey
					status, err := c.valkeyClient.GetEndpointStatus(ctx, chain, endpointID)
					if err == nil {
						switch protocol {
						case "http":
							status.HealthyHTTP = true
						case "ws":
							status.HealthyWS = true
						}
						c.updateStatus(ctx, chain, endpointID, *status)
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
					log.Debug().Str("chain", chain).Str("endpoint_id", endpointID).Str("protocol", protocol).Msg("Ephemeral check failed, resetting counter")
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
		for endpointID, endpoint := range endpoints {
			go c.checkEndpoint(ctx, chain, endpointID, endpoint)
		}
	}
}

// checkAllEndpointsAndWait checks all endpoints and waits for all checks to complete
// If isInitial is true, it marks the checker as ready after completion
func (c *Checker) checkAllEndpointsAndWait(ctx context.Context, isInitial bool) {
	var wg sync.WaitGroup

	// Create a semaphore channel to limit concurrency
	semaphore := make(chan struct{}, c.concurrency)

	for chain, endpoints := range c.config.Endpoints {
		for endpointID, endpoint := range endpoints {
			wg.Add(1)
			go func(chain, endpointID string, endpoint config.Endpoint) {
				defer wg.Done()

				// Acquire semaphore (blocks if at capacity)
				semaphore <- struct{}{}
				defer func() { <-semaphore }() // Release semaphore

				c.checkEndpoint(ctx, chain, endpointID, endpoint)
			}(chain, endpointID, endpoint)
		}
	}

	// Wait for all endpoint checks to complete
	wg.Wait()

	// Mark as ready if this is the initial check
	if isInitial {
		c.readyMu.Lock()
		c.isReady = true
		c.readyMu.Unlock()
		c.initialCheckDone.Done()
		log.Info().Msg("Initial health check completed, service is ready")
	}
}

// checkEndpoint checks the health of a single endpoint
func (c *Checker) checkEndpoint(ctx context.Context, chain, endpointID string, endpoint config.Endpoint) {
	status := store.NewEndpointStatus()
	status.LastHealthCheck = time.Now()

	// Create channels to collect results from parallel health checks
	httpResult := make(chan bool, 1)
	wsResult := make(chan bool, 1)

	// Run HTTP health check in parallel
	go func() {
		healthy := c.CheckHTTPHealthFunc(ctx, chain, endpointID, endpoint)
		httpResult <- healthy
	}()

	// Run WS health check in parallel
	go func() {
		healthy := c.CheckWSHealthFunc(ctx, chain, endpointID, endpoint)
		wsResult <- healthy
	}()

	// Collect results
	status.HasHTTP = endpoint.HTTPURL != ""
	status.HasWS = endpoint.WSURL != ""
	status.HealthyHTTP = <-httpResult
	status.HealthyWS = <-wsResult

	// Get current request counts
	r24h, r1m, rAll, err := c.valkeyClient.GetCombinedRequestCounts(ctx, chain, endpointID)
	if err == nil {
		status.Requests24h = r24h
		status.Requests1Month = r1m
		status.RequestsLifetime = rAll
	}

	// Update status in Valkey
	c.updateStatus(ctx, chain, endpointID, status)
}

// makeRPCCall makes a single JSON-RPC call and returns the result
func (c *Checker) makeRPCCall(ctx context.Context, url, method, chain, endpointID string) (any, error) {
	payload := []byte(`{"jsonrpc":"2.0","method":"` + method + `","params":[],"id":1}`)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(payload))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// Check for "bad" HTTP status codes
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Handle 429 (Too Many Requests) specially
		if resp.StatusCode == 429 && c.HandleRateLimitFunc != nil {
			log.Debug().
				Str("endpoint", helpers.RedactAPIKey(url)).
				Str("chain", chain).
				Str("endpoint_id", endpointID).
				Int("status_code", resp.StatusCode).
				Str("method", method).
				Msg("RPC call detected 429, handing over to rate limit handler")

			c.HandleRateLimitFunc(chain, endpointID, "http")
		}

		// Read and log up to 512 bytes of the failed response's body
		bodyBytes := make([]byte, 512)
		n, _ := io.ReadFull(resp.Body, bodyBytes)
		log.Error().
			Str("endpoint", helpers.RedactAPIKey(url)).
			Str("chain", chain).
			Str("endpoint_id", endpointID).
			Str("method", method).
			Int("status_code", resp.StatusCode).
			Str("body", string(bodyBytes[:n])).
			Msg("RPC call failed: endpoint returned non-2xx status")
		return nil, errors.New("HTTP " + strconv.Itoa(resp.StatusCode) + ": " + resp.Status)
	}

	// Read response
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Error().
			Err(err).
			Str("chain", chain).
			Str("endpoint", helpers.RedactAPIKey(url)).
			Str("endpoint_id", endpointID).
			Str("method", method).
			Msg("RPC call failed: could not read response body")
		return nil, err
	}

	// Define the structure of the response
	var response rpcResponse

	// Parse the response
	if err := json.Unmarshal(body, &response); err != nil {
		log.Error().
			Err(err).
			Str("body", string(body)).
			Str("endpoint", helpers.RedactAPIKey(url)).
			Str("chain", chain).
			Str("endpoint_id", endpointID).
			Str("method", method).
			Msg("RPC call failed: could not parse JSON-RPC response")
		return nil, err
	}

	// Check for errors inside the response
	if err := checkRPCError(&response, method, "HTTP", chain, endpointID, url); err != nil {
		return nil, err
	}

	return response.Result, nil
}

// makeWSRPCCall makes a single JSON-RPC call over WebSocket and returns the result
func (c *Checker) makeWSRPCCall(url, method, chain, endpointID string) (any, error) {
	wsDialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	wsConn, _, err := wsDialer.Dial(url, nil)
	if err != nil {
		return nil, err
	}
	defer wsConn.Close()

	// Create JSON-RPC request
	request := map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  []any{},
		"id":      1,
	}

	// Send the request
	if err := wsConn.WriteJSON(request); err != nil {
		log.Error().
			Err(err).
			Str("endpoint", helpers.RedactAPIKey(url)).
			Str("chain", chain).
			Str("endpoint_id", endpointID).
			Str("method", method).
			Msg("WS RPC call failed: could not send request")
		return nil, err
	}

	// Set read deadline
	wsConn.SetReadDeadline(time.Now().Add(5 * time.Second))

	// Read the response
	var response rpcResponse

	if err := wsConn.ReadJSON(&response); err != nil {
		log.Error().
			Err(err).
			Str("endpoint", helpers.RedactAPIKey(url)).
			Str("chain", chain).
			Str("endpoint_id", endpointID).
			Str("method", method).
			Msg("WS RPC call failed: could not read response")
		return nil, err
	}

	// Check for errors inside the response
	if err := checkRPCError(&response, method, "WS", chain, endpointID, url); err != nil {
		return nil, err
	}

	return response.Result, nil
}

// parseBlockNumber parses a hex string block number and validates it's > 0
func parseBlockNumber(blockResult any) (blockNumber int64, isHealthy bool) {
	blockStr, ok := blockResult.(string)
	if !ok {
		return 0, false
	}

	// Parse hex string to int64
	if len(blockStr) >= 3 && blockStr[:2] == "0x" {
		if parsed, err := strconv.ParseInt(blockStr[2:], 16, 64); err == nil {
			blockNumber = parsed
			// The node is healthy if block number > 0
			isHealthy = blockNumber > 0
			return blockNumber, isHealthy
		}
	}

	return 0, false
}

// parseSyncStatus checks if the node is syncing
// A node is considered to be healthy if result is false (i.e., node is not syncing)
func parseSyncStatus(syncResult any) bool {
	if result, ok := syncResult.(bool); ok {
		// result = true means syncing (unhealthy), result = false means not syncing (healthy)
		return !result
	}

	// If result is an object or any other value, assume the node is syncing (unhealthy)
	return false
}

// checkHealthParams checks all health parameters and logs detailed info
func (c *Checker) checkHealthParams(chain, endpointID, url, protocol string, syncResult, blockResult any) (healthy bool, blockNumber int64) {
	// Parse results
	blockNumber, blockHealthy := parseBlockNumber(blockResult)

	// Block number check is always required
	healthy = blockHealthy

	// Add sync check if enabled
	if c.healthCheckSyncStatus {
		// If syncResult is nil (method not supported), assume node is healthy (not syncing)
		syncHealthy := true
		if syncResult != nil {
			syncHealthy = parseSyncStatus(syncResult)
		}
		healthy = healthy && syncHealthy

		if healthy {
			log.Debug().
				Int64("block_number", blockNumber).
				Str("chain", chain).
				Str("endpoint", helpers.RedactAPIKey(url)).
				Str("endpoint_id", endpointID).
				Str("protocol", protocol).
				Msg("Health check succeeded: node is not syncing and has a valid block number")
		} else {
			reason := "node "
			if !blockHealthy {
				reason += "returned an invalid block number (" + strconv.FormatInt(blockNumber, 10) + ")"
			}
			if !syncHealthy {
				if reason != "node " {
					reason += " and "
				}
				reason += "is syncing"
			}
			log.Error().
				Str("chain", chain).
				Bool("block_healthy", blockHealthy).
				Int64("block_number", blockNumber).
				Str("endpoint", helpers.RedactAPIKey(url)).
				Str("endpoint_id", endpointID).
				Str("protocol", protocol).
				Str("reason", reason).
				Bool("sync_healthy", syncHealthy).
				Msg("Health check failed")
		}
	} else {
		if healthy {
			log.Debug().
				Int64("block_number", blockNumber).
				Str("chain", chain).
				Str("endpoint", helpers.RedactAPIKey(url)).
				Str("endpoint_id", endpointID).
				Str("protocol", protocol).
				Msg("Health check succeeded: valid block number (sync check disabled)")
		} else {
			log.Error().
				Str("chain", chain).
				Bool("block_healthy", blockHealthy).
				Int64("block_number", blockNumber).
				Str("endpoint", helpers.RedactAPIKey(url)).
				Str("endpoint_id", endpointID).
				Str("protocol", protocol).
				Str("reason", "node returned an invalid block number ("+strconv.FormatInt(blockNumber, 10)+")").
				Msg("Health check failed (sync check disabled)")
		}
	}

	return healthy, blockNumber
}

// updateHealthMetrics updates health-related metrics for an endpoint
func (c *Checker) updateHealthMetrics(chain, endpointID string, healthy bool) {
	if healthy {
		metrics.EndpointHealthStatus.WithLabelValues(chain, endpointID).Set(1)
		metrics.HealthCheckTotal.WithLabelValues(chain, endpointID, "success").Inc()
	} else {
		metrics.EndpointHealthStatus.WithLabelValues(chain, endpointID).Set(0)
		metrics.HealthCheckTotal.WithLabelValues(chain, endpointID, "failure").Inc()
	}
}

// incrementHealthRequestCount increments the health request count and logs errors
func (c *Checker) incrementHealthRequestCount(ctx context.Context, chain, endpointID string) {
	if err := c.valkeyClient.IncrementRequestCount(ctx, chain, endpointID, "health_requests"); err != nil {
		log.Error().Err(err).Str("chain", chain).Str("endpoint_id", endpointID).Msg("Failed to increment health request count")
	}
}

// updateEndpointStatusInValkey fetches current status, updates it with new values, and stores it in Valkey
func (c *Checker) updateEndpointStatusInValkey(ctx context.Context, chain, endpointID string, updateFn func(*store.EndpointStatus)) {
	status, err := c.valkeyClient.GetEndpointStatus(ctx, chain, endpointID)
	if err != nil || status == nil {
		st := store.NewEndpointStatus()
		status = &st
	}

	// Apply the update function
	updateFn(status)

	// Get current request counts
	r24h, r1m, rAll, err := c.valkeyClient.GetCombinedRequestCounts(ctx, chain, endpointID)
	if err == nil {
		status.Requests24h = r24h
		status.Requests1Month = r1m
		status.RequestsLifetime = rAll
	}

	// Update in Valkey
	c.updateStatus(ctx, chain, endpointID, *status)
}

// checkHTTPHealth performs HTTP health check and returns health status
func (c *Checker) checkHTTPHealth(ctx context.Context, chain, endpointID string, endpoint config.Endpoint) bool {
	if endpoint.HTTPURL == "" {
		return false // No HTTP endpoint to check
	}

	// Start timer for metrics
	timer := prometheus.NewTimer(metrics.HealthCheckDuration.WithLabelValues(chain, endpointID))
	defer timer.ObserveDuration()

	log.Info().Str("chain", chain).Str("endpoint_id", endpointID).Str("url", helpers.RedactAPIKey(endpoint.HTTPURL)).Msg("Running HTTP health check")

	// Always make the eth_blockNumber call
	blockResult, blockErr := c.makeRPCCall(ctx, endpoint.HTTPURL, "eth_blockNumber", chain, endpointID)
	c.incrementHealthRequestCount(ctx, chain, endpointID)

	// Only make the eth_syncing call if sync status checking is enabled
	var syncResult any
	var syncErr error
	if c.healthCheckSyncStatus {
		syncResult, syncErr = c.makeRPCCall(ctx, endpoint.HTTPURL, "eth_syncing", chain, endpointID)
		c.incrementHealthRequestCount(ctx, chain, endpointID)
	}

	// If eth_blockNumber call failed, the endpoint is unhealthy
	if blockErr != nil {
		c.updateHealthMetrics(chain, endpointID, false)
		return false
	}

	// If sync status checking is enabled and eth_syncing failed (but not due to method not found), the endpoint is unhealthy
	if c.healthCheckSyncStatus && syncErr != nil && !errors.Is(syncErr, ErrMethodNotFound) {
		c.updateHealthMetrics(chain, endpointID, false)
		return false
	}

	// Check all health parameters
	healthy, blockNumber := c.checkHealthParams(chain, endpointID, endpoint.HTTPURL, "HTTP", syncResult, blockResult)

	// Update metrics and status in Valkey
	c.updateHealthMetrics(chain, endpointID, healthy)
	c.updateEndpointStatusInValkey(ctx, chain, endpointID, func(status *store.EndpointStatus) {
		status.BlockNumber = blockNumber // Store the block number for future reference
		status.HasHTTP = endpoint.HTTPURL != ""
		status.HealthyHTTP = healthy
		status.LastHealthCheck = time.Now()
	})
	return healthy
}

// checkWSHealth performs WebSocket health checks and returns its health status
func (c *Checker) checkWSHealth(ctx context.Context, chain, endpointID string, endpoint config.Endpoint) bool {
	if endpoint.WSURL == "" {
		return false // No WS endpoint to check
	}

	// Start timer for metrics
	timer := prometheus.NewTimer(metrics.HealthCheckDuration.WithLabelValues(chain, endpointID))
	defer timer.ObserveDuration()

	log.Info().Str("chain", chain).Str("endpoint_id", endpointID).Str("url", helpers.RedactAPIKey(endpoint.WSURL)).Msg("Running WS health check")

	// Always make the eth_blockNumber call
	blockResult, blockErr := c.makeWSRPCCall(endpoint.WSURL, "eth_blockNumber", chain, endpointID)
	c.incrementHealthRequestCount(ctx, chain, endpointID)

	// Only make the eth_syncing call if sync status checking is enabled
	var syncResult any
	var syncErr error
	if c.healthCheckSyncStatus {
		syncResult, syncErr = c.makeWSRPCCall(endpoint.WSURL, "eth_syncing", chain, endpointID)
		c.incrementHealthRequestCount(ctx, chain, endpointID)
	}

	// If eth_blockNumber call failed, the endpoint is unhealthy
	if blockErr != nil {
		c.updateHealthMetrics(chain, endpointID, false)
		return false
	}

	// If sync status checking is enabled and eth_syncing failed (but not due to method not found), the endpoint is unhealthy
	if c.healthCheckSyncStatus && syncErr != nil && !errors.Is(syncErr, ErrMethodNotFound) {
		c.updateHealthMetrics(chain, endpointID, false)
		return false
	}

	// Check all health parameters
	healthy, blockNumber := c.checkHealthParams(chain, endpointID, endpoint.WSURL, "WS", syncResult, blockResult)

	// Update metrics and status in Valkey
	c.updateHealthMetrics(chain, endpointID, healthy)
	c.updateEndpointStatusInValkey(ctx, chain, endpointID, func(status *store.EndpointStatus) {
		status.BlockNumber = blockNumber // Store the block number for future reference
		status.HasWS = endpoint.WSURL != ""
		status.HealthyWS = healthy
		status.LastHealthCheck = time.Now()
	})
	return healthy
}

// updateStatus updates the endpoint status in Valkey
func (c *Checker) updateStatus(ctx context.Context, chain, endpointID string, status store.EndpointStatus) {
	if err := c.valkeyClient.UpdateEndpointStatus(ctx, chain, endpointID, status); err != nil {
		log.Error().Err(err).
			Str("chain", chain).
			Str("endpoint_id", endpointID).
			Msg("Failed to update endpoint status")
	} else {
		log.Info().
			Str("chain", chain).
			Str("endpoint_id", endpointID).
			Bool("has_http", status.HasHTTP).
			Bool("has_ws", status.HasWS).
			Bool("healthy_http", status.HealthyHTTP).
			Bool("healthy_ws", status.HealthyWS).
			Msg("Updated endpoint status")
	}
}
