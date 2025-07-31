package main

import (
	"aetherlay/internal/config"
	"aetherlay/internal/health"
	"aetherlay/internal/store"
	"context"
	"testing"
)

// mockConfig returns a minimal valid *config.Config for testing
func mockConfig() *config.Config {
	return &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"mainnet": {
				"mock": config.Endpoint{
					Provider: "mock",
					Role:     "primary",
					Type:     "full",
					HTTPURL:  "http://mock",
					WSURL:    "ws://mock",
				},
			},
		},
	}
}

// TestRunHealthCheckerFromEnv_Standalone tests standalone mode detection in main.
func TestRunHealthCheckerFromEnv_Standalone(t *testing.T) {
	testExitAfterSetup = true
	defer func() { testExitAfterSetup = false }()

	// Patch newRedisClient
	newRedisClient = func(addr string, password string) store.RedisClientIface {
		return store.NewMockRedisClient()
	}
	defer func() {
		newRedisClient = func(addr string, password string) store.RedisClientIface { return store.NewRedisClient(addr, password) }
	}()

	// Patch loadConfig
	loadConfig = func(path string) (*config.Config, error) {
		return mockConfig(), nil
	}
	defer func() { loadConfig = config.LoadConfig }()

	// Patch health check methods to always return healthy
	testCheckerPatch = func(checker *health.Checker) {
		checker.CheckHTTPHealthFunc = func(ctx context.Context, chain, endpointID string, endpoint config.Endpoint) bool { return true }
		checker.CheckWSHealthFunc = func(ctx context.Context, chain, endpointID string, endpoint config.Endpoint) bool { return true }
	}
	defer func() { testCheckerPatch = nil }()

	var detectedMode string
	onModeDetected = func(mode string) {
		detectedMode = mode
	}

	// Call RunHealthChecker directly
	RunHealthChecker(
		"mock",      // configFile
		30,          // ephemeralChecksInterval
		3,           // ephemeralChecksHealthyThreshold
		30,          // healthCheckInterval
		"localhost", // redisHost
		"6379",      // redisPort
		"",          // redisPassword
		true,        // standaloneHealthChecks
	)

	if detectedMode != "standalone" {
		t.Errorf("Expected mode 'standalone', got '%s'", detectedMode)
	}
}

// TestRunHealthCheckerFromEnv_Ephemeral tests ephemeral mode detection in main.
func TestRunHealthCheckerFromEnv_Ephemeral(t *testing.T) {
	testExitAfterSetup = true
	defer func() { testExitAfterSetup = false }()

	// Patch newRedisClient
	newRedisClient = func(addr string, password string) store.RedisClientIface {
		return store.NewMockRedisClient()
	}
	defer func() {
		newRedisClient = func(addr string, password string) store.RedisClientIface { return store.NewRedisClient(addr, password) }
	}()

	// Patch loadConfig
	loadConfig = func(path string) (*config.Config, error) {
		return mockConfig(), nil
	}
	defer func() { loadConfig = config.LoadConfig }()

	// Patch health check methods to always return healthy
	testCheckerPatch = func(checker *health.Checker) {
		checker.CheckHTTPHealthFunc = func(ctx context.Context, chain, endpointID string, endpoint config.Endpoint) bool { return true }
		checker.CheckWSHealthFunc = func(ctx context.Context, chain, endpointID string, endpoint config.Endpoint) bool { return true }
	}
	defer func() { testCheckerPatch = nil }()

	var detectedMode string
	onModeDetected = func(mode string) {
		detectedMode = mode
	}

	// Call RunHealthChecker directly
	RunHealthChecker(
		"mock",      // configFile
		30,          // ephemeralChecksInterval
		3,           // ephemeralChecksHealthyThreshold
		0,           // healthCheckInterval (ephemeral mode)
		"localhost", // redisHost
		"6379",      // redisPort
		"",          // redisPassword
		true,        // standaloneHealthChecks
	)

	if detectedMode != "ephemeral" {
		t.Errorf("Expected mode 'ephemeral', got '%s'", detectedMode)
	}
}

// TestRunHealthCheckerFromEnv_Disabled tests disabled mode detection in main.
func TestRunHealthCheckerFromEnv_Disabled(t *testing.T) {
	testExitAfterSetup = true
	defer func() { testExitAfterSetup = false }()

	// Patch newRedisClient
	newRedisClient = func(addr string, password string) store.RedisClientIface {
		return store.NewMockRedisClient()
	}
	defer func() {
		newRedisClient = func(addr string, password string) store.RedisClientIface { return store.NewRedisClient(addr, password) }
	}()

	// Patch loadConfig
	loadConfig = func(path string) (*config.Config, error) {
		return mockConfig(), nil
	}
	defer func() { loadConfig = config.LoadConfig }()

	// Patch health check methods to always return healthy
	testCheckerPatch = func(checker *health.Checker) {
		checker.CheckHTTPHealthFunc = func(ctx context.Context, chain, endpointID string, endpoint config.Endpoint) bool { return true }
		checker.CheckWSHealthFunc = func(ctx context.Context, chain, endpointID string, endpoint config.Endpoint) bool { return true }
	}
	defer func() { testCheckerPatch = nil }()

	var detectedMode string
	onModeDetected = func(mode string) {
		detectedMode = mode
	}

	// Call RunHealthChecker directly
	RunHealthChecker(
		"mock",      // configFile
		30,          // ephemeralChecksInterval
		3,           // ephemeralChecksHealthyThreshold
		0,           // healthCheckInterval (doesn't matter)
		"localhost", // redisHost
		"6379",      // redisPort
		"",          // redisPassword
		false,       // standaloneHealthChecks (disabled)
	)

	if detectedMode != "disabled" {
		t.Errorf("Expected mode 'disabled', got '%s'", detectedMode)
	}
}
