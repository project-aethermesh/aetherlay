package config

import (
	"encoding/json"
	"os"
)

// RateLimitRecovery represents the configuration for rate limit recovery
type RateLimitRecovery struct {
	BackoffMultiplier float64 `json:"backoff_multiplier"` // Multiplier for exponential backoff (e.g., 2.0)
	InitialBackoff    int     `json:"initial_backoff"`    // Initial backoff time in seconds
	MaxBackoff        int     `json:"max_backoff"`        // Maximum backoff time in seconds
	MaxRetries        int     `json:"max_retries"`        // Maximum number of recovery attempts
	RequiredSuccesses int     `json:"required_successes"` // Number of consecutive successes needed to mark as recovered
	ResetAfter        int     `json:"reset_after"`        // Time in seconds after which to reset backoff and start from scratch
}

// CapacityLimit represents a self-imposed throughput ceiling for an endpoint, used to
// proactively throttle requests before the provider's own rate limiter would trigger.
// There is no default: a numeric ceiling is only meaningful relative to an operator's
// specific paid plan with that specific provider, so it must be explicitly configured.
type CapacityLimit struct {
	MaxRequests   int `json:"max_requests"`   // Requests allowed per window
	WindowSeconds int `json:"window_seconds"` // Width of the window, in seconds
}

// CapacityLearning tunes the AIMD control loop used to adaptively estimate an
// endpoint's safe throughput ceiling from observed rate-limit hits, when no static
// CapacityLimit is configured. See DefaultCapacityLearning for the default values.
type CapacityLearning struct {
	DecreaseFactor   float64 `json:"decrease_factor"`   // Multiplier applied to the ceiling on a confirmed rate-limit hit (e.g. 0.5 halves it)
	IncreaseInterval int     `json:"increase_interval"` // Seconds of sustained clean time per additive-increase step
	MinEstimate      int     `json:"min_estimate"`      // Floor the learned ceiling can never decrease below
	WindowSeconds    int     `json:"window_seconds"`    // Default learning window width, used when there's no static CapacityLimit to inherit one from
}

// Endpoint represents a single RPC endpoint configuration.
// It contains all the necessary information to connect to and use an RPC provider.
type Endpoint struct {
	Provider          string             `json:"provider"`            // Name of the RPC provider (e.g., "alchemy", "infura")
	RateLimitRecovery *RateLimitRecovery `json:"rate_limit_recovery"` // Rate limit recovery configuration (optional)
	Capacity          *CapacityLimit     `json:"capacity"`            // Self-imposed throughput ceiling (optional; nil disables proactive throttling)
	CapacityLearning  *CapacityLearning  `json:"capacity_learning"`   // Adaptive capacity learning tuning override (optional; only used when Capacity is unset)
	Role              string             `json:"role"`                // Role of the endpoint: "primary" or "fallback"
	SkipSyncCheck     bool               `json:"skip_sync_check"`     // Skip eth_syncing check for this endpoint (default: false)
	Type              string             `json:"type"`                // Type of node: "full" or "archive"
	HTTPURL           string             `json:"http_url"`            // HTTP/HTTPS URL for RPC requests
	WSURL             string             `json:"ws_url"`              // WebSocket URL for real-time connections
}

// ChainEndpoints represents all endpoints for a specific blockchain.
// The key is the endpoint ID, and the value is the endpoint configuration.
type ChainEndpoints map[string]Endpoint

// Config represents the entire configuration structure for the load balancer.
// It contains all endpoint configurations organized by blockchain name.
type Config struct {
	Endpoints map[string]ChainEndpoints `json:"-"` // Map of chain name to its endpoints
}

// substituteEnvVars replaces ${VAR_NAME} patterns with environment variable values.
// This allows for dynamic configuration using environment variables.
func substituteEnvVars(s string) string {
	return os.Expand(s, func(key string) string {
		return os.Getenv(key)
	})
}

// substituteEnvVarsInEndpoint recursively substitutes environment variables in an endpoint.
// It processes both HTTPURL and WSURL fields for environment variable substitution.
func substituteEnvVarsInEndpoint(endpoint *Endpoint) {
	endpoint.HTTPURL = substituteEnvVars(endpoint.HTTPURL)
	endpoint.WSURL = substituteEnvVars(endpoint.WSURL)
}

// LoadConfig loads the configuration from a JSON file.
// It reads the file, parses the JSON, and substitutes environment variables in all URLs.
// Returns an error if the file cannot be read or if the JSON is invalid.
func LoadConfig(path string) (*Config, error) {
	file, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var config Config
	if err := json.Unmarshal(file, &config.Endpoints); err != nil {
		return nil, err
	}

	// Substitute environment variables in all endpoints
	for chainName, chainEndpoints := range config.Endpoints {
		for endpointID, endpoint := range chainEndpoints {
			substituteEnvVarsInEndpoint(&endpoint)
			config.Endpoints[chainName][endpointID] = endpoint
		}
	}

	return &config, nil
}

// GetEndpointsForChain returns all endpoints for a specific chain.
// Returns the endpoints and a boolean indicating if the chain exists.
func (c *Config) GetEndpointsForChain(chain string) (ChainEndpoints, bool) {
	endpoints, exists := c.Endpoints[chain]
	return endpoints, exists
}

// GetPrimaryEndpoints returns all primary endpoints for a chain.
// Primary endpoints are used first for load balancing before falling back to fallback endpoints.
// Returns nil if the chain doesn't exist or has no primary endpoints.
func (c *Config) GetPrimaryEndpoints(chain string) []Endpoint {
	endpoints, exists := c.Endpoints[chain]
	if !exists {
		return nil
	}

	var primaryEndpoints []Endpoint
	for _, endpoint := range endpoints {
		if endpoint.Role == "primary" {
			primaryEndpoints = append(primaryEndpoints, endpoint)
		}
	}
	return primaryEndpoints
}

// GetFallbackEndpoints returns all fallback endpoints for a chain.
// Fallback endpoints are used when primary endpoints are unavailable.
// Returns nil if the chain doesn't exist or has no fallback endpoints.
func (c *Config) GetFallbackEndpoints(chain string) []Endpoint {
	endpoints, exists := c.Endpoints[chain]
	if !exists {
		return nil
	}

	var fallbackEndpoints []Endpoint
	for _, endpoint := range endpoints {
		if endpoint.Role == "fallback" {
			fallbackEndpoints = append(fallbackEndpoints, endpoint)
		}
	}
	return fallbackEndpoints
}

// GetPublicEndpoints returns all public endpoints for a chain.
// Public endpoints are free/public RPC nodes that can be prioritized when PUBLIC_FIRST is enabled.
// Returns nil if the chain doesn't exist or has no public endpoints.
func (c *Config) GetPublicEndpoints(chain string) []Endpoint {
	endpoints, exists := c.Endpoints[chain]
	if !exists {
		return nil
	}

	var publicEndpoints []Endpoint
	for _, endpoint := range endpoints {
		if endpoint.Role == "public" {
			publicEndpoints = append(publicEndpoints, endpoint)
		}
	}
	return publicEndpoints
}

// DefaultRateLimitRecovery returns the default rate limit recovery configuration
func DefaultRateLimitRecovery() RateLimitRecovery {
	return RateLimitRecovery{
		BackoffMultiplier: 2.0,   // Double the backoff each time
		InitialBackoff:    300,   // Start with 300 seconds
		MaxBackoff:        7200,  // Cap at 2 hours
		MaxRetries:        10,    // Up to 10 recovery attempts
		RequiredSuccesses: 2,     // Need 2 consecutive successes to mark the endpoint as recovered
		ResetAfter:        86400, // Reset backoff after 1 day
	}
}

// DefaultCapacityLearning returns the default adaptive capacity learning configuration.
func DefaultCapacityLearning() CapacityLearning {
	return CapacityLearning{
		DecreaseFactor:   0.5, // Halve the estimate on a confirmed rate-limit hit
		IncreaseInterval: 60,  // Grow once per minute of sustained clean traffic
		MinEstimate:      1,   // Never learn a ceiling below 1 request/window
		WindowSeconds:    60,  // Default learning window when no static CapacityLimit exists
	}
}

// ResolveCapacityLearning merges an optional per-endpoint override onto the package
// defaults, replacing only the fields the operator explicitly set (non-zero) - mirrors
// the RateLimitRecovery merge pattern used in rate_limit_scheduler.go.
func ResolveCapacityLearning(override *CapacityLearning) CapacityLearning {
	resolved := DefaultCapacityLearning()
	if override == nil {
		return resolved
	}
	if override.DecreaseFactor != 0 {
		resolved.DecreaseFactor = override.DecreaseFactor
	}
	if override.IncreaseInterval != 0 {
		resolved.IncreaseInterval = override.IncreaseInterval
	}
	if override.MinEstimate != 0 {
		resolved.MinEstimate = override.MinEstimate
	}
	if override.WindowSeconds != 0 {
		resolved.WindowSeconds = override.WindowSeconds
	}
	return resolved
}
