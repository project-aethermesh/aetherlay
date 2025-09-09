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
	newRedisClient = func(addr string, password string, skipTLSVerify bool, redisUseTLS bool) store.RedisClientIface {
		return store.NewMockRedisClient()
	}
	defer func() {
		newRedisClient = func(addr string, password string, skipTLSVerify bool, redisUseTLS bool) store.RedisClientIface {
			return store.NewRedisClient(addr, password, skipTLSVerify, redisUseTLS)
		}
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
		"mock", // configFile
		"Accept, Authorization, Content-Type, Origin, X-Requested-With", // corsHeaders
		"GET, POST, OPTIONS", // corsMethods
		"*",                  // corsOrigin
		3,                    // ephemeralChecksHealthyThreshold
		30,                   // ephemeralChecksInterval
		30,                   // healthCheckInterval
		true,                 // healthCheckSyncStatus
		false,                // metricsEnabled
		9090,                 // metricsPort
		"localhost",          // redisHost
		"",                   // redisPass
		"6379",               // redisPort
		false,                // redisSkipTLSCheck
		false,                // redisUseTLS
		true,                 // standaloneHealthChecks
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
	newRedisClient = func(addr string, password string, skipTLSVerify bool, redisUseTLS bool) store.RedisClientIface {
		return store.NewMockRedisClient()
	}
	defer func() {
		newRedisClient = func(addr string, password string, skipTLSVerify bool, redisUseTLS bool) store.RedisClientIface {
			return store.NewRedisClient(addr, password, skipTLSVerify, redisUseTLS)
		}
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
		"mock", // configFile
		"Accept, Authorization, Content-Type, Origin, X-Requested-With", // corsHeaders
		"GET, POST, OPTIONS", // corsMethods
		"*",                  // corsOrigin
		3,                    // ephemeralChecksHealthyThreshold
		30,                   // ephemeralChecksInterval
		0,                    // healthCheckInterval (ephemeral mode)
		true,                 // healthCheckSyncStatus
		false,                // metricsEnabled
		9090,                 // metricsPort
		"localhost",          // redisHost
		"",                   // redisPass
		"6379",               // redisPort
		false,                // redisSkipTLSCheck
		false,                // redisUseTLS
		true,                 // standaloneHealthChecks
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
	newRedisClient = func(addr string, password string, skipTLSVerify bool, redisUseTLS bool) store.RedisClientIface {
		return store.NewMockRedisClient()
	}
	defer func() {
		newRedisClient = func(addr string, password string, skipTLSVerify bool, redisUseTLS bool) store.RedisClientIface {
			return store.NewRedisClient(addr, password, skipTLSVerify, redisUseTLS)
		}
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
		"mock", // configFile
		"Accept, Authorization, Content-Type, Origin, X-Requested-With", // corsHeaders
		"GET, POST, OPTIONS", // corsMethods
		"*",                  // corsOrigin
		3,                    // ephemeralChecksHealthyThreshold
		30,                   // ephemeralChecksInterval
		0,                    // healthCheckInterval (doesn't matter)
		true,                 // healthCheckSyncStatus
		false,                // metricsEnabled
		9090,                 // metricsPort
		"localhost",          // redisHost
		"",                   // redisPass
		"6379",               // redisPort
		false,                // redisSkipTLSCheck
		false,                // redisUseTLS
		false,                // standaloneHealthChecks (disabled)
	)

	if detectedMode != "disabled" {
		t.Errorf("Expected mode 'disabled', got '%s'", detectedMode)
	}
}
