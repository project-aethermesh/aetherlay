package health

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"aetherlay/internal/store"
)

func TestNewHealthChecker(t *testing.T) {
	redisClient := store.NewMockRedisClient()
	checker := NewHealthChecker(redisClient)

	if checker == nil {
		t.Fatal("HealthChecker should not be nil")
	}

	if checker.RedisClient != redisClient {
		t.Error("Redis client should be set correctly")
	}
}

func TestCheckHealthWithHealthyEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("healthy"))
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	redisClient := store.NewMockRedisClient()
	checker := NewHealthChecker(redisClient)

	err := checker.CheckHealth(server.URL)
	if err != nil {
		t.Fatalf("Health check should succeed for healthy endpoint: %v", err)
	}

	healthCheck, err := checker.GetHealthStatus(server.URL)
	if err != nil {
		t.Fatalf("Failed to get health status from Redis: %v", err)
	}

	if !healthCheck.HealthStatus {
		t.Error("Health status should be true for healthy endpoint")
	}

	if healthCheck.EndpointURL != server.URL {
		t.Errorf("Expected endpoint URL '%s', got '%s'", server.URL, healthCheck.EndpointURL)
	}
}

func TestCheckHealthWithUnhealthyEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("unhealthy"))
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	redisClient := store.NewMockRedisClient()
	checker := NewHealthChecker(redisClient)

	err := checker.CheckHealth(server.URL)
	if err != nil {
		t.Fatalf("Health check should succeed even for unhealthy endpoint: %v", err)
	}

	healthCheck, err := checker.GetHealthStatus(server.URL)
	if err != nil {
		t.Fatalf("Failed to get health status from Redis: %v", err)
	}

	if healthCheck.HealthStatus {
		t.Error("Health status should be false for unhealthy endpoint")
	}
}

func TestCheckHealthWithNetworkError(t *testing.T) {
	redisClient := store.NewMockRedisClient()
	checker := NewHealthChecker(redisClient)

	err := checker.CheckHealth("https://non-existent-domain-that-will-fail.com")
	if err == nil {
		t.Error("Health check should fail for non-existent domain")
	}
}

func TestCheckHealthWithInvalidURL(t *testing.T) {
	redisClient := store.NewMockRedisClient()
	checker := NewHealthChecker(redisClient)

	err := checker.CheckHealth("invalid-url")
	if err == nil {
		t.Error("Health check should fail for invalid URL")
	}
}

func TestGetHealthStatusFromRedis(t *testing.T) {
	redisClient := store.NewMockRedisClient()
	checker := NewHealthChecker(redisClient)

	_, err := checker.GetHealthStatus("https://non-existent-endpoint.com")
	if err == nil {
		t.Error("Should return error for non-existent health status")
	}
}

func TestHealthCheckSerialization(t *testing.T) {
	healthCheck := HealthCheck{
		EndpointURL:  "https://example.com",
		HealthStatus: true,
		LastChecked:  time.Now(),
	}

	data, err := json.Marshal(healthCheck)
	if err != nil {
		t.Fatalf("Failed to marshal health check: %v", err)
	}

	var unmarshaled HealthCheck
	err = json.Unmarshal(data, &unmarshaled)
	if err != nil {
		t.Fatalf("Failed to unmarshal health check: %v", err)
	}

	if unmarshaled.EndpointURL != healthCheck.EndpointURL {
		t.Errorf("Expected endpoint URL '%s', got '%s'", healthCheck.EndpointURL, unmarshaled.EndpointURL)
	}

	if unmarshaled.HealthStatus != healthCheck.HealthStatus {
		t.Errorf("Expected health status %t, got %t", healthCheck.HealthStatus, unmarshaled.HealthStatus)
	}
}

func TestHealthCheckWithTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			time.Sleep(2 * time.Second)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("healthy"))
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	redisClient := store.NewMockRedisClient()
	checker := NewHealthChecker(redisClient)

	err := checker.CheckHealth(server.URL)
	if err == nil {
		t.Error("Health check should timeout for slow endpoint")
	}
}
