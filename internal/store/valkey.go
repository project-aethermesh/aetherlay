package store

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"time"

	"github.com/valkey-io/valkey-go"
)

const (
	// Key prefixes for Valkey storage
	healthPrefix    = "health:"
	metricsPrefix   = "metrics:"
	rateLimitPrefix = "rate_limit:"
	proxyRequests   = "proxy_requests"
	healthRequests  = "health_requests"
	requests24hKey  = "requests_24h"
	requests1mKey   = "requests_1m"
	requestsAllKey  = "requests_all"
)

// EndpointStatus represents the health status and metrics of an endpoint.
// It contains information about the endpoint's health, protocol support, and request counts.
type EndpointStatus struct {
	LastHealthCheck  time.Time `json:"last_health_check"` // When the last health check was performed
	Requests24h      int64     `json:"requests_24h"`      // Number of requests in the last 24 hours
	Requests1Month   int64     `json:"requests_1_month"`  // Number of requests in the last month
	RequestsLifetime int64     `json:"requests_lifetime"` // Total number of requests since start

	// Protocol support and health flags
	HasHTTP     bool `json:"has_http"`     // Whether the endpoint supports HTTP/HTTPS
	HasWS       bool `json:"has_ws"`       // Whether the endpoint supports WebSocket
	HealthyHTTP bool `json:"healthy_http"` // Whether the HTTP endpoint is healthy
	HealthyWS   bool `json:"healthy_ws"`   // Whether the WebSocket endpoint is healthy

	// Blockchain state information
	BlockNumber int64 `json:"block_number"` // Latest block number from eth_blockNumber
}

// NewEndpointStatus creates a new endpoint status with default values.
// All health flags are set to false and request counts are initialized to 0.
func NewEndpointStatus() EndpointStatus {
	return EndpointStatus{
		LastHealthCheck:  time.Now(),
		Requests24h:      0,
		Requests1Month:   0,
		RequestsLifetime: 0,
		HasHTTP:          false,
		HasWS:            false,
		HealthyHTTP:      false,
		HealthyWS:        false,
		BlockNumber:      0,
	}
}

// ValkeyClientIface defines the interface for Valkey operations used by the server.
// This allows for mocking in tests and provides a clean separation of concerns.
// Only include methods actually used by the server.
type ValkeyClientIface interface {
	GetEndpointStatus(ctx context.Context, chain, endpoint string) (*EndpointStatus, error)
	UpdateEndpointStatus(ctx context.Context, chain, endpoint string, status EndpointStatus) error
	IncrementRequestCount(ctx context.Context, chain, endpoint string, requestType string) error
	GetCombinedRequestCounts(ctx context.Context, chain, endpoint string) (int64, int64, int64, error)
	GetRateLimitState(ctx context.Context, chain, endpoint string) (*RateLimitState, error)
	SetRateLimitState(ctx context.Context, chain, endpoint string, state RateLimitState) error
	Ping(ctx context.Context) error
	Close() error
}

// ValkeyClient wraps the Valkey client with our custom methods.
// It implements ValkeyClientIface and provides methods for storing and retrieving
// endpoint health status and request metrics.
type ValkeyClient struct {
	client valkey.Client
}

// NewValkeyClient creates a new Valkey client with optimized connection settings.
// It configures connection pooling, timeouts, and retry logic for production use.
func NewValkeyClient(addr string, password string, skipTLSVerify bool, useTLS bool) *ValkeyClient {
	var tlsConfig *tls.Config
	if useTLS {
		tlsConfig = &tls.Config{
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: skipTLSVerify,
		}
	}

	client, err := valkey.NewClient(valkey.ClientOption{
		InitAddress:      []string{addr},
		Password:         password,
		TLSConfig:        tlsConfig,
		ConnWriteTimeout: 10 * time.Second,
		ConnLifetime:     30 * time.Minute,
	})
	if err != nil {
		panic(err)
	}
	return &ValkeyClient{client: client}
}

// Ping checks the Valkey connection by sending a PING command.
// Returns an error if the connection cannot be established.
func (r *ValkeyClient) Ping(ctx context.Context) error {
	cmd := r.client.B().Ping().Build()
	return r.client.Do(ctx, cmd).Error()
}

// Close closes the Valkey connection and releases all resources.
func (r *ValkeyClient) Close() error {
	r.client.Close()
	return nil
}

// UpdateEndpointStatus updates the health status of an endpoint in Valkey.
// The data is stored as JSON with the key pattern "health:{chain}:{endpoint}".
// The data has no expiration and persists until explicitly deleted.
func (r *ValkeyClient) UpdateEndpointStatus(ctx context.Context, chain, endpoint string, status EndpointStatus) error {
	key := healthPrefix + chain + ":" + endpoint
	data, err := json.Marshal(status)
	if err != nil {
		return err
	}
	cmd := r.client.B().Set().Key(key).Value(string(data)).Build()
	return r.client.Do(ctx, cmd).Error()
}

// GetEndpointStatus retrieves the health status of an endpoint from Valkey.
// If the endpoint doesn't exist in Valkey, it creates a new status with default values.
// Returns the endpoint status and any error that occurred during retrieval.
func (r *ValkeyClient) GetEndpointStatus(ctx context.Context, chain, endpoint string) (*EndpointStatus, error) {
	key := healthPrefix + chain + ":" + endpoint
	cmd := r.client.B().Get().Key(key).Build()
	result := r.client.Do(ctx, cmd)

	if valkey.IsValkeyNil(result.Error()) {
		// Initialize new endpoint status if it doesn't exist
		status := NewEndpointStatus()
		if err := r.UpdateEndpointStatus(ctx, chain, endpoint, status); err != nil {
			return nil, err
		}
		return &status, nil
	}

	data, err := result.AsBytes()
	if err != nil {
		return nil, err
	}

	var status EndpointStatus
	if err := json.Unmarshal(data, &status); err != nil {
		return nil, err
	}
	return &status, nil
}

// RateLimitState represents the rate limit recovery state for an endpoint
type RateLimitState struct {
	ConsecutiveSuccess int       `json:"consecutive_success"` // Number of consecutive successful recovery checks
	CurrentBackoff     int       `json:"current_backoff"`     // Current backoff time in seconds
	FirstRateLimited   time.Time `json:"first_rate_limited"`  // When the endpoint was first rate limited (for reset_after)
	LastRecoveryCheck  time.Time `json:"last_recovery_check"` // When the last recovery check was performed
	RateLimited        bool      `json:"rate_limited"`        // Whether the endpoint is currently rate limited
	RecoveryAttempts   int       `json:"recovery_attempts"`   // Number of recovery attempts made
}

// IncrementRequestCount increments the request count for an endpoint in Valkey.
// It maintains separate counters for 24-hour, 1-month, and lifetime requests.
// The 24-hour and 1-month counters have automatic expiration set.
func (r *ValkeyClient) IncrementRequestCount(ctx context.Context, chain, endpoint string, requestType string) error {
	key := metricsPrefix + chain + ":" + endpoint + ":" + requestType

	// Build all commands
	cmds := []valkey.Completed{
		r.client.B().Incr().Key(key + ":" + requests24hKey).Build(),
		r.client.B().Expire().Key(key + ":" + requests24hKey).Seconds(int64((24 * time.Hour).Seconds())).Build(),
		r.client.B().Incr().Key(key + ":" + requests1mKey).Build(),
		r.client.B().Expire().Key(key + ":" + requests1mKey).Seconds(int64((30 * 24 * time.Hour).Seconds())).Build(),
		r.client.B().Incr().Key(key + ":" + requestsAllKey).Build(),
	}

	// Execute all commands in a pipeline
	results := r.client.DoMulti(ctx, cmds...)
	for _, result := range results {
		if err := result.Error(); err != nil {
			return err
		}
	}
	return nil
}

// GetRequestCounts gets the request counts for an endpoint.
// Returns the 24-hour, 1-month, and lifetime request counts.
// If any counter doesn't exist, it returns 0 for that counter.
func (r *ValkeyClient) GetRequestCounts(ctx context.Context, chain, endpoint string, requestType string) (int64, int64, int64, error) {
	key := metricsPrefix + chain + ":" + endpoint + ":" + requestType

	// Build all GET commands
	cmds := []valkey.Completed{
		r.client.B().Get().Key(key + ":" + requests24hKey).Build(),
		r.client.B().Get().Key(key + ":" + requests1mKey).Build(),
		r.client.B().Get().Key(key + ":" + requestsAllKey).Build(),
	}

	// Execute all commands in a pipeline
	results := r.client.DoMulti(ctx, cmds...)

	var r24h, r1m, rAll int64

	// Parse each result, treating nil as 0
	if !valkey.IsValkeyNil(results[0].Error()) {
		r24h, _ = results[0].AsInt64()
	}
	if !valkey.IsValkeyNil(results[1].Error()) {
		r1m, _ = results[1].AsInt64()
	}
	if !valkey.IsValkeyNil(results[2].Error()) {
		rAll, _ = results[2].AsInt64()
	}

	return r24h, r1m, rAll, nil
}

// GetCombinedRequestCounts gets the combined request counts (proxy + health) for an endpoint.
// This is useful for monitoring total endpoint usage including health check requests.
// Returns the combined 24-hour, 1-month, and lifetime request counts.
func (r *ValkeyClient) GetCombinedRequestCounts(ctx context.Context, chain, endpoint string) (int64, int64, int64, error) {
	// Get proxy request counts
	p24h, p1m, pAll, err := r.GetRequestCounts(ctx, chain, endpoint, proxyRequests)
	if err != nil {
		p24h, p1m, pAll = 0, 0, 0
	}

	// Get health request counts
	h24h, h1m, hAll, err := r.GetRequestCounts(ctx, chain, endpoint, healthRequests)
	if err != nil {
		h24h, h1m, hAll = 0, 0, 0
	}

	// Return combined counts
	return p24h + h24h, p1m + h1m, pAll + hAll, nil
}

// GetRateLimitState retrieves the rate limit state for an endpoint from Valkey
func (r *ValkeyClient) GetRateLimitState(ctx context.Context, chain, endpoint string) (*RateLimitState, error) {
	key := rateLimitPrefix + chain + ":" + endpoint
	cmd := r.client.B().Get().Key(key).Build()
	result := r.client.Do(ctx, cmd)

	if valkey.IsValkeyNil(result.Error()) {
		// Return default state if not found
		return &RateLimitState{
			ConsecutiveSuccess: 0,
			CurrentBackoff:     0,
			FirstRateLimited:   time.Time{},
			LastRecoveryCheck:  time.Time{},
			RateLimited:        false,
			RecoveryAttempts:   0,
		}, nil
	}

	data, err := result.AsBytes()
	if err != nil {
		return nil, err
	}

	var state RateLimitState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

// SetRateLimitState updates the rate limit state for an endpoint in Valkey
func (r *ValkeyClient) SetRateLimitState(ctx context.Context, chain, endpoint string, state RateLimitState) error {
	key := rateLimitPrefix + chain + ":" + endpoint
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	// Set with 24-hour expiration to prevent indefinite storage of old rate limit states
	cmd := r.client.B().Set().Key(key).Value(string(data)).Ex(24 * time.Hour).Build()
	return r.client.Do(ctx, cmd).Error()
}
