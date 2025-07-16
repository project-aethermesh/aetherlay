package health

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"aetherlay/internal/store"
)

// HealthCheck represents the health status of an endpoint
type HealthCheck struct {
	EndpointURL  string    `json:"endpoint_url"`
	HealthStatus bool      `json:"health_status"`
	LastChecked  time.Time `json:"last_checked"`
}

// HealthChecker represents a health checking service
type HealthChecker struct {
	RedisClient store.RedisClientIface
}

// NewHealthChecker creates a new health checker instance
func NewHealthChecker(redisClient store.RedisClientIface) *HealthChecker {
	return &HealthChecker{RedisClient: redisClient}
}

// CheckHealth performs a health check on the specified endpoint
func (hc *HealthChecker) CheckHealth(endpointURL string) error {
	// Create HTTP client with timeout
	client := &http.Client{
		Timeout: 1 * time.Second,
	}
	
	resp, err := client.Get(endpointURL + "/health")
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	healthStatus := resp.StatusCode == http.StatusOK
	now := time.Now()

	healthCheck := HealthCheck{
		EndpointURL:  endpointURL,
		HealthStatus: healthStatus,
		LastChecked:  now,
	}

	return hc.updateHealthStatusInRedis(healthCheck)
}

// updateHealthStatusInRedis stores the health check result in Redis
func (hc *HealthChecker) updateHealthStatusInRedis(healthCheck HealthCheck) error {
	ctx := context.Background()
	data, err := json.Marshal(healthCheck)
	if err != nil {
		return err
	}

	// Use the endpoint URL as the key for storing health status
	return hc.RedisClient.(interface {
		Set(ctx context.Context, key string, value interface{}, expiration time.Duration) error
	}).Set(ctx, healthCheck.EndpointURL, data, 0)
}

// GetHealthStatus retrieves the health status of an endpoint from Redis
func (hc *HealthChecker) GetHealthStatus(endpointURL string) (HealthCheck, error) {
	ctx := context.Background()
	data, err := hc.RedisClient.(interface {
		Get(ctx context.Context, key string) (string, error)
	}).Get(ctx, endpointURL)
	if err != nil {
		return HealthCheck{}, err
	}

	var healthCheck HealthCheck
	err = json.Unmarshal([]byte(data), &healthCheck)
	return healthCheck, err
}
