package config

import (
	"encoding/json"
	"os"
)

// Endpoint represents a single RPC endpoint configuration.
// It contains all the necessary information to connect to and use an RPC provider.
type Endpoint struct {
	Provider string `json:"provider"` // Name of the RPC provider (e.g., "alchemy", "infura")
	Role     string `json:"role"`     // Role of the endpoint: "primary" or "fallback"
	Type     string `json:"type"`     // Type of node: "full" or "archive"
	Weight   int    `json:"weight"`   // Weight for load balancing (higher = more requests)
	RPCURL   string `json:"rpc_url"`  // HTTP/HTTPS URL for RPC requests
	WSURL    string `json:"ws_url"`   // WebSocket URL for real-time connections
}

// ChainEndpoints represents all endpoints for a specific blockchain.
// The key is the provider name, and the value is the endpoint configuration.
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
// It processes both RPCURL and WSURL fields for environment variable substitution.
func substituteEnvVarsInEndpoint(endpoint *Endpoint) {
	endpoint.RPCURL = substituteEnvVars(endpoint.RPCURL)
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
