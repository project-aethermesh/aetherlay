package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"aetherlay/internal/config"
	"aetherlay/internal/store"
)

func TestNewRateLimitScheduler(t *testing.T) {
	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"ethereum": {
				"test-endpoint": config.Endpoint{
					Provider: "test-provider",
					Role:     "primary",
					Type:     "full",
					HTTPURL:  "http://test-url.com",
				},
			},
		},
	}

	mockValkey := store.NewMockValkeyClient()
	scheduler := NewRateLimitScheduler(cfg, mockValkey)

	if scheduler == nil {
		t.Fatal("Expected scheduler to be created")
	}

	if scheduler.config != cfg {
		t.Error("Expected config to be set")
	}

	if scheduler.valkeyClient != mockValkey {
		t.Error("Expected Valkey client to be set")
	}

	if scheduler.activeMonitoring == nil {
		t.Error("Expected active monitoring map to be initialized")
	}
}

func TestStartMonitoringDoesNotDuplicate(t *testing.T) {
	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"ethereum": {
				"test-endpoint": config.Endpoint{
					Provider: "test-provider",
					Role:     "primary",
					Type:     "full",
					HTTPURL:  "http://test-url.com",
				},
			},
		},
	}

	mockValkey := store.NewMockValkeyClient()
	scheduler := NewRateLimitScheduler(cfg, mockValkey)

	// Start monitoring
	scheduler.StartMonitoring("ethereum", "test-endpoint")

	// Verify it's being monitored
	key := "ethereum:test-endpoint"
	scheduler.mu.RLock()
	if !scheduler.activeMonitoring[key] {
		t.Error("Expected endpoint to be actively monitored")
	}
	scheduler.mu.RUnlock()

	// Start monitoring again - should not create duplicate
	initialCount := len(scheduler.activeMonitoring)
	scheduler.StartMonitoring("ethereum", "test-endpoint")

	scheduler.mu.RLock()
	newCount := len(scheduler.activeMonitoring)
	scheduler.mu.RUnlock()

	if newCount != initialCount {
		t.Error("Expected duplicate monitoring to be prevented")
	}
}

func TestCheckEndpointHealthSuccess(t *testing.T) {
	// Create a test HTTP server that returns 200
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"ethereum": {
				"test-endpoint": config.Endpoint{
					Provider: "test-provider",
					Role:     "primary",
					Type:     "full",
					HTTPURL:  server.URL,
				},
			},
		},
	}

	mockValkey := store.NewMockValkeyClient()
	scheduler := NewRateLimitScheduler(cfg, mockValkey)

	endpoint := cfg.Endpoints["ethereum"]["test-endpoint"]
	healthy := scheduler.checkEndpointHealth(endpoint)

	if !healthy {
		t.Error("Expected endpoint to be healthy")
	}
}

func TestCheckEndpointHealthRateLimited(t *testing.T) {
	// Create a test HTTP server that returns 429
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"ethereum": {
				"test-endpoint": config.Endpoint{
					Provider: "test-provider",
					Role:     "primary",
					Type:     "full",
					HTTPURL:  server.URL,
				},
			},
		},
	}

	mockValkey := store.NewMockValkeyClient()
	scheduler := NewRateLimitScheduler(cfg, mockValkey)

	endpoint := cfg.Endpoints["ethereum"]["test-endpoint"]
	healthy := scheduler.checkEndpointHealth(endpoint)

	if healthy {
		t.Error("Expected endpoint to be unhealthy due to rate limiting")
	}
}

func TestPerformRecoveryCheckStopsWhenNotRateLimited(t *testing.T) {
	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"ethereum": {
				"test-endpoint": config.Endpoint{
					Provider: "test-provider",
					Role:     "primary",
					Type:     "full",
					HTTPURL:  "http://test-url.com",
				},
			},
		},
	}

	mockValkey := store.NewMockValkeyClient()

	// Set up endpoint as not rate limited
	state := store.RateLimitState{
		RateLimited:        false,
		RecoveryAttempts:   0,
		LastRecoveryCheck:  time.Time{},
		ConsecutiveSuccess: 0,
	}
	mockValkey.SetRateLimitState(context.Background(), "ethereum", "test-endpoint", state)

	scheduler := NewRateLimitScheduler(cfg, mockValkey)

	endpoint := cfg.Endpoints["ethereum"]["test-endpoint"]
	rateLimitConfig := config.DefaultRateLimitRecovery()

	shouldContinue, err := scheduler.performRecoveryCheck("ethereum", "test-endpoint", endpoint, rateLimitConfig)

	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	if shouldContinue {
		t.Error("Expected monitoring to stop when endpoint is not rate limited")
	}
}

func TestPerformRecoveryCheckStopsAfterMaxRetries(t *testing.T) {
	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"ethereum": {
				"test-endpoint": config.Endpoint{
					Provider: "test-provider",
					Role:     "primary",
					Type:     "full",
					HTTPURL:  "http://test-url.com",
				},
			},
		},
	}

	mockValkey := store.NewMockValkeyClient()

	// Set up endpoint as rate limited with max retries reached
	state := store.RateLimitState{
		ConsecutiveSuccess: 0,
		CurrentBackoff:     120,
		FirstRateLimited:   time.Now().Add(-10 * time.Minute),
		LastRecoveryCheck:  time.Now(),
		RateLimited:        true,
		RecoveryAttempts:   5, // At max retries
	}
	mockValkey.SetRateLimitState(context.Background(), "ethereum", "test-endpoint", state)

	scheduler := NewRateLimitScheduler(cfg, mockValkey)

	endpoint := cfg.Endpoints["ethereum"]["test-endpoint"]
	rateLimitConfig := config.RateLimitRecovery{
		BackoffMultiplier: 2.0,
		InitialBackoff:    30,
		MaxBackoff:        300,
		MaxRetries:        5, // Same as recovery attempts
		RequiredSuccesses: 3,
		ResetAfter:        3600,
	}

	shouldContinue, err := scheduler.performRecoveryCheck("ethereum", "test-endpoint", endpoint, rateLimitConfig)

	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	if shouldContinue {
		t.Error("Expected monitoring to stop when max retries reached")
	}
}

func TestPerformRecoveryCheckRecovery(t *testing.T) {
	// Create a test HTTP server that returns 200
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"ethereum": {
				"test-endpoint": config.Endpoint{
					Provider: "test-provider",
					Role:     "primary",
					Type:     "full",
					HTTPURL:  server.URL,
				},
			},
		},
	}

	mockValkey := store.NewMockValkeyClient()

	// Set up initial endpoint status
	endpointStatus := store.EndpointStatus{
		HasHTTP:     true,
		HealthyHTTP: false, // Currently unhealthy
	}
	mockValkey.UpdateEndpointStatus(context.Background(), "ethereum", "test-endpoint", endpointStatus)

	// Set up endpoint as rate limited with enough consecutive successes to recover
	state := store.RateLimitState{
		ConsecutiveSuccess: 2, // One away from recovery
		CurrentBackoff:     60,
		FirstRateLimited:   time.Now().Add(-5 * time.Minute),
		LastRecoveryCheck:  time.Now(),
		RateLimited:        true,
		RecoveryAttempts:   2,
	}
	mockValkey.SetRateLimitState(context.Background(), "ethereum", "test-endpoint", state)

	scheduler := NewRateLimitScheduler(cfg, mockValkey)

	endpoint := cfg.Endpoints["ethereum"]["test-endpoint"]
	rateLimitConfig := config.RateLimitRecovery{
		BackoffMultiplier: 2.0,
		InitialBackoff:    30,
		MaxBackoff:        300,
		MaxRetries:        10,
		RequiredSuccesses: 3, // Need 3 successes, we have 2
		ResetAfter:        3600,
	}

	shouldContinue, err := scheduler.performRecoveryCheck("ethereum", "test-endpoint", endpoint, rateLimitConfig)

	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	if shouldContinue {
		t.Error("Expected monitoring to stop after recovery")
	}

	// Verify endpoint was marked as recovered
	finalState, err := mockValkey.GetRateLimitState(context.Background(), "ethereum", "test-endpoint")
	if err != nil {
		t.Fatalf("Failed to get final rate limit state: %v", err)
	}

	if finalState.RateLimited {
		t.Error("Expected endpoint to be marked as not rate limited after recovery")
	}

	// Verify endpoint status was updated
	finalStatus, err := mockValkey.GetEndpointStatus(context.Background(), "ethereum", "test-endpoint")
	if err != nil {
		t.Fatalf("Failed to get final endpoint status: %v", err)
	}

	if !finalStatus.HealthyHTTP {
		t.Error("Expected endpoint to be marked as healthy after recovery")
	}
}

func TestCalculateNextBackoff(t *testing.T) {
	cfg := &config.Config{}
	mockValkey := store.NewMockValkeyClient()
	scheduler := NewRateLimitScheduler(cfg, mockValkey)

	config := config.RateLimitRecovery{
		InitialBackoff: 30,
		MaxBackoff:     300,
	}

	// Test initial backoff
	state := &store.RateLimitState{CurrentBackoff: 0}
	backoff := scheduler.calculateNextBackoff(state, config)
	if backoff != 30 {
		t.Errorf("Expected initial backoff to be 30, got %d", backoff)
	}

	// Test current backoff
	state.CurrentBackoff = 60
	backoff = scheduler.calculateNextBackoff(state, config)
	if backoff != 60 {
		t.Errorf("Expected current backoff to be 60, got %d", backoff)
	}
}

func TestShouldResetBackoff(t *testing.T) {
	cfg := &config.Config{}
	mockValkey := store.NewMockValkeyClient()
	scheduler := NewRateLimitScheduler(cfg, mockValkey)

	config := config.RateLimitRecovery{
		ResetAfter: 3600, // 1 hour
	}

	// Test no reset needed (recent)
	state := &store.RateLimitState{
		FirstRateLimited: time.Now().Add(-30 * time.Minute), // 30 minutes ago
	}
	shouldReset := scheduler.shouldResetBackoff(state, config)
	if shouldReset {
		t.Error("Expected no reset for recent rate limit")
	}

	// Test reset needed (old)
	state.FirstRateLimited = time.Now().Add(-2 * time.Hour) // 2 hours ago
	shouldReset = scheduler.shouldResetBackoff(state, config)
	if !shouldReset {
		t.Error("Expected reset for old rate limit")
	}

	// Test with zero time (no reset)
	state.FirstRateLimited = time.Time{}
	shouldReset = scheduler.shouldResetBackoff(state, config)
	if shouldReset {
		t.Error("Expected no reset for zero time")
	}
}
