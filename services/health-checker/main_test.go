package main

import (
	"aetherlay/internal/config"
	"aetherlay/internal/health"
	"aetherlay/internal/store"
	"context"
	"testing"
	"time"
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

	// Patch newValkeyClient
	newValkeyClient = func(addr string, password string, skipTLSVerify bool, valkeyUseTLS bool) store.ValkeyClientIface {
		return store.NewMockValkeyClient()
	}
	defer func() {
		newValkeyClient = func(addr string, password string, skipTLSVerify bool, valkeyUseTLS bool) store.ValkeyClientIface {
			return store.NewValkeyClient(addr, password, skipTLSVerify, valkeyUseTLS)
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
		true,                 // capacityLearningEnabled
		true,                 // capacityThrottlingEnabled
		true,                 // ephemeralChecksEnabled
		3,                    // ephemeralChecksHealthyThreshold
		30,                   // ephemeralChecksInterval
		20,                   // healthCheckConcurrency
		30,                   // healthCheckInterval
		true,                 // healthCheckSyncStatus
		8080,                 // healthCheckerServerPort
		false,                // metricsEnabled
		9090,                 // metricsPort
		"localhost",          // valkeyHost
		"",                   // valkeyPass
		"6379",               // valkeyPort
		false,                // valkeySkipTLSCheck
		false,                // valkeyUseTLS
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

	// Patch newValkeyClient
	newValkeyClient = func(addr string, password string, skipTLSVerify bool, valkeyUseTLS bool) store.ValkeyClientIface {
		return store.NewMockValkeyClient()
	}
	defer func() {
		newValkeyClient = func(addr string, password string, skipTLSVerify bool, valkeyUseTLS bool) store.ValkeyClientIface {
			return store.NewValkeyClient(addr, password, skipTLSVerify, valkeyUseTLS)
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
		true,                 // capacityLearningEnabled
		true,                 // capacityThrottlingEnabled
		true,                 // ephemeralChecksEnabled
		3,                    // ephemeralChecksHealthyThreshold
		30,                   // ephemeralChecksInterval
		20,                   // healthCheckConcurrency
		0,                    // healthCheckInterval (ephemeral mode)
		true,                 // healthCheckSyncStatus
		8080,                 // healthCheckerServerPort
		false,                // metricsEnabled
		9090,                 // metricsPort
		"localhost",          // valkeyHost
		"",                   // valkeyPass
		"6379",               // valkeyPort
		false,                // valkeySkipTLSCheck
		false,                // valkeyUseTLS
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

	// Patch newValkeyClient
	newValkeyClient = func(addr string, password string, skipTLSVerify bool, valkeyUseTLS bool) store.ValkeyClientIface {
		return store.NewMockValkeyClient()
	}
	defer func() {
		newValkeyClient = func(addr string, password string, skipTLSVerify bool, valkeyUseTLS bool) store.ValkeyClientIface {
			return store.NewValkeyClient(addr, password, skipTLSVerify, valkeyUseTLS)
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
		true,                 // capacityLearningEnabled
		true,                 // capacityThrottlingEnabled
		true,                 // ephemeralChecksEnabled
		3,                    // ephemeralChecksHealthyThreshold
		30,                   // ephemeralChecksInterval
		20,                   // healthCheckConcurrency
		0,                    // healthCheckInterval (doesn't matter)
		true,                 // healthCheckSyncStatus
		8080,                 // healthCheckerServerPort
		false,                // metricsEnabled
		9090,                 // metricsPort
		"localhost",          // valkeyHost
		"",                   // valkeyPass
		"6379",               // valkeyPort
		false,                // valkeySkipTLSCheck
		false,                // valkeyUseTLS
		false,                // standaloneHealthChecks (disabled)
	)

	if detectedMode != "disabled" {
		t.Errorf("Expected mode 'disabled', got '%s'", detectedMode)
	}
}

// TestStandaloneInitialBackoff tests that standaloneInitialBackoff mirrors
// server.Server.initialBackoffForSignal's precedence: Retry-After first, then the
// endpoint's own (or default) MaxBackoff for a daily-quota signal, else 0.
func TestStandaloneInitialBackoff(t *testing.T) {
	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"mainnet": {
				"with-override": config.Endpoint{
					Provider:          "infura",
					Role:              "primary",
					Type:              "full",
					HTTPURL:           "http://with-override",
					RateLimitRecovery: &config.RateLimitRecovery{MaxBackoff: 555},
				},
				"no-override": config.Endpoint{
					Provider: "infura",
					Role:     "primary",
					Type:     "full",
					HTTPURL:  "http://no-override",
				},
			},
		},
	}

	tests := []struct {
		name       string
		endpointID string
		signal     health.RateLimitSignal
		expected   int
	}{
		{"retry-after takes priority over daily quota", "no-override", health.RateLimitSignal{RetryAfter: 20 * time.Second, IsDailyQuota: true}, 20},
		{"daily quota uses endpoint's own MaxBackoff override", "with-override", health.RateLimitSignal{IsDailyQuota: true}, 555},
		{"daily quota without override uses default MaxBackoff", "no-override", health.RateLimitSignal{IsDailyQuota: true}, config.DefaultRateLimitRecovery().MaxBackoff},
		{"plain rate limit signal leaves 0", "no-override", health.RateLimitSignal{IsRateLimited: true}, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := standaloneInitialBackoff(cfg, "mainnet", tt.endpointID, tt.signal); got != tt.expected {
				t.Errorf("standaloneInitialBackoff() = %d, want %d", got, tt.expected)
			}
		})
	}
}

// TestCreateStandaloneRateLimitHandlerSeedsBackoffFromSignal tests that the standalone
// health checker's rate limit handler seeds CurrentBackoff from the signal, not just
// marking the endpoint rate limited with a zero backoff.
func TestCreateStandaloneRateLimitHandlerSeedsBackoffFromSignal(t *testing.T) {
	cfg := mockConfig()
	valkeyClient := store.NewMockValkeyClient()
	handler := createStandaloneRateLimitHandler(cfg, valkeyClient, true, true)

	handler("mainnet", "mock", "http", health.RateLimitSignal{RetryAfter: 15 * time.Second})

	state, err := valkeyClient.GetRateLimitState(context.Background(), "mainnet", "mock")
	if err != nil {
		t.Fatalf("Failed to get rate limit state: %v", err)
	}
	if !state.RateLimited {
		t.Error("Expected endpoint to be marked rate limited")
	}
	if state.CurrentBackoff != 15 {
		t.Errorf("Expected CurrentBackoff to be seeded to 15 from Retry-After, got %d", state.CurrentBackoff)
	}
}

// TestApplyStandaloneLearnedCapacityDecreaseSeedsFromObservedCount confirms the
// standalone health checker's decrease path shares the exact same store.ApplyCapacityDecrease
// math as the load balancer's own applyLearnedCapacityDecrease - load-bearing, since
// these run as separate processes mutating the same Valkey-persisted estimate.
func TestApplyStandaloneLearnedCapacityDecreaseSeedsFromObservedCount(t *testing.T) {
	cfg := mockConfig()
	valkeyClient := store.NewMockValkeyClient()
	ctx := context.Background()

	for i := 0; i < 10; i++ {
		valkeyClient.IncrementCapacityCount(ctx, "mainnet", "mock", 60)
	}

	applyStandaloneLearnedCapacityDecrease(cfg, valkeyClient, true, true, "mainnet", "mock", health.RateLimitSignal{})

	estimate, err := valkeyClient.GetCapacityEstimate(ctx, "mainnet", "mock")
	if err != nil {
		t.Fatalf("GetCapacityEstimate failed: %v", err)
	}
	if !estimate.HasEstimate || estimate.MaxRequests != 5 {
		t.Errorf("Expected a learned estimate of 5 (10 observed * 0.5), got %+v", estimate)
	}
}

// TestApplyStandaloneLearnedCapacityDecreaseSkipsWhenStaticCapacityConfigured mirrors
// server.effectiveCapacityCeiling's rule: adaptive learning never engages for an
// endpoint that already has a static Capacity configured.
func TestApplyStandaloneLearnedCapacityDecreaseSkipsWhenStaticCapacityConfigured(t *testing.T) {
	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"mainnet": {
				"mock": config.Endpoint{
					Provider: "mock", Role: "primary", Type: "full", HTTPURL: "http://mock",
					Capacity: &config.CapacityLimit{MaxRequests: 100, WindowSeconds: 10},
				},
			},
		},
	}
	valkeyClient := store.NewMockValkeyClient()
	ctx := context.Background()

	applyStandaloneLearnedCapacityDecrease(cfg, valkeyClient, true, true, "mainnet", "mock", health.RateLimitSignal{})

	estimate, _ := valkeyClient.GetCapacityEstimate(ctx, "mainnet", "mock")
	if estimate.HasEstimate {
		t.Error("Expected no learned estimate when a static Capacity is configured")
	}
}

// TestCreateStandaloneRateLimitHandlerAlsoSeedsCapacityEstimate confirms the handler
// wires both the reactive backoff and the proactive learned-estimate decrease together.
func TestCreateStandaloneRateLimitHandlerAlsoSeedsCapacityEstimate(t *testing.T) {
	cfg := mockConfig()
	valkeyClient := store.NewMockValkeyClient()
	ctx := context.Background()

	for i := 0; i < 8; i++ {
		valkeyClient.IncrementCapacityCount(ctx, "mainnet", "mock", 60)
	}

	handler := createStandaloneRateLimitHandler(cfg, valkeyClient, true, true)
	handler("mainnet", "mock", "http", health.RateLimitSignal{IsRateLimited: true})

	estimate, err := valkeyClient.GetCapacityEstimate(ctx, "mainnet", "mock")
	if err != nil {
		t.Fatalf("GetCapacityEstimate failed: %v", err)
	}
	if !estimate.HasEstimate || estimate.MaxRequests != 4 {
		t.Errorf("Expected a learned estimate of 4 (8 observed * 0.5), got %+v", estimate)
	}
}

// TestCreateStandaloneRateLimitHandlerSkipsCapacityEstimateForDailyQuota mirrors
// TestApplyLearnedCapacityDecreaseSkipsForDailyQuotaSignal in the server package, but
// through the standalone health checker's own production entry point
// (createStandaloneRateLimitHandler), not just the shared store function directly - this
// is what actually confirms the daily-quota exclusion is wired through this process's
// handler, not only proven correct in the load balancer's call path.
func TestCreateStandaloneRateLimitHandlerSkipsCapacityEstimateForDailyQuota(t *testing.T) {
	cfg := mockConfig()
	valkeyClient := store.NewMockValkeyClient()
	ctx := context.Background()

	for i := 0; i < 8; i++ {
		valkeyClient.IncrementCapacityCount(ctx, "mainnet", "mock", 60)
	}

	handler := createStandaloneRateLimitHandler(cfg, valkeyClient, true, true)
	handler("mainnet", "mock", "http", health.RateLimitSignal{IsRateLimited: true, IsDailyQuota: true})

	estimate, err := valkeyClient.GetCapacityEstimate(ctx, "mainnet", "mock")
	if err != nil {
		t.Fatalf("GetCapacityEstimate failed: %v", err)
	}
	if estimate.HasEstimate {
		t.Errorf("Expected no learned estimate to be seeded from a daily-quota (402) signal, got %+v", estimate)
	}
}
