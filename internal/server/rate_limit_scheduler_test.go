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

	mockRedis := store.NewMockRedisClient()
	scheduler := NewRateLimitScheduler(cfg, mockRedis)

	if scheduler == nil {
		t.Error("Expected scheduler to be created")
	}

	if scheduler.config != cfg {
		t.Error("Expected config to be set")
	}

	if scheduler.redisClient != mockRedis {
		t.Error("Expected redis client to be set")
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

	mockRedis := store.NewMockRedisClient()
	scheduler := NewRateLimitScheduler(cfg, mockRedis)

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

	mockRedis := store.NewMockRedisClient()
	scheduler := NewRateLimitScheduler(cfg, mockRedis)

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

	mockRedis := store.NewMockRedisClient()
	scheduler := NewRateLimitScheduler(cfg, mockRedis)

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

	mockRedis := store.NewMockRedisClient()
	
	// Set up endpoint as not rate limited
	state := store.RateLimitState{
		RateLimited:        false,
		RecoveryAttempts:   0,
		LastRecoveryCheck:  time.Time{},
		ConsecutiveSuccess: 0,
	}
	mockRedis.SetRateLimitState(context.Background(), "ethereum", "test-endpoint", state)

	scheduler := NewRateLimitScheduler(cfg, mockRedis)

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

	mockRedis := store.NewMockRedisClient()
	
	// Set up endpoint as rate limited with max retries reached
	state := store.RateLimitState{
		RateLimited:        true,
		RecoveryAttempts:   5, // At max retries
		LastRecoveryCheck:  time.Now(),
		ConsecutiveSuccess: 0,
	}
	mockRedis.SetRateLimitState(context.Background(), "ethereum", "test-endpoint", state)

	scheduler := NewRateLimitScheduler(cfg, mockRedis)

	endpoint := cfg.Endpoints["ethereum"]["test-endpoint"]
	rateLimitConfig := config.RateLimitRecovery{
		CheckInterval:     60,
		MaxRetries:        5, // Same as recovery attempts
		RequiredSuccesses: 3,
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

	mockRedis := store.NewMockRedisClient()
	
	// Set up initial endpoint status
	endpointStatus := store.EndpointStatus{
		HasHTTP:     true,
		HealthyHTTP: false, // Currently unhealthy
	}
	mockRedis.UpdateEndpointStatus(context.Background(), "ethereum", "test-endpoint", endpointStatus)

	// Set up endpoint as rate limited with enough consecutive successes to recover
	state := store.RateLimitState{
		RateLimited:        true,
		RecoveryAttempts:   2,
		LastRecoveryCheck:  time.Now(),
		ConsecutiveSuccess: 2, // One away from recovery
	}
	mockRedis.SetRateLimitState(context.Background(), "ethereum", "test-endpoint", state)

	scheduler := NewRateLimitScheduler(cfg, mockRedis)

	endpoint := cfg.Endpoints["ethereum"]["test-endpoint"]
	rateLimitConfig := config.RateLimitRecovery{
		CheckInterval:     60,
		MaxRetries:        10,
		RequiredSuccesses: 3, // Need 3 successes, we have 2
	}

	shouldContinue, err := scheduler.performRecoveryCheck("ethereum", "test-endpoint", endpoint, rateLimitConfig)

	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	if shouldContinue {
		t.Error("Expected monitoring to stop after recovery")
	}

	// Verify endpoint was marked as recovered
	finalState, err := mockRedis.GetRateLimitState(context.Background(), "ethereum", "test-endpoint")
	if err != nil {
		t.Fatalf("Failed to get final rate limit state: %v", err)
	}

	if finalState.RateLimited {
		t.Error("Expected endpoint to be marked as not rate limited after recovery")
	}

	// Verify endpoint status was updated
	finalStatus, err := mockRedis.GetEndpointStatus(context.Background(), "ethereum", "test-endpoint")
	if err != nil {
		t.Fatalf("Failed to get final endpoint status: %v", err)
	}

	if !finalStatus.HealthyHTTP {
		t.Error("Expected endpoint to be marked as healthy after recovery")
	}
}