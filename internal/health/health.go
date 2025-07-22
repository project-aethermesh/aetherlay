package health

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// Check represents the health status of an endpoint
type Check struct {
	EndpointURL  string    `json:"endpoint_url"`
	HealthStatus bool      `json:"health_status"`
	LastChecked  time.Time `json:"last_checked"`
}

// CheckHealth performs a health check on the specified endpoint
func (hc *Checker) CheckHealth(endpointURL string) error {
	// Create HTTP client with timeout
	client := &http.Client{
		Timeout: 5 * time.Second,
	}
	resp, err := client.Get(endpointURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	healthStatus := resp.StatusCode == http.StatusOK
	now := time.Now()

	check := Check{
		EndpointURL:  endpointURL,
		HealthStatus: healthStatus,
		LastChecked:  now,
	}

	return hc.updateHealthStatusInRedis(check)
}

// updateHealthStatusInRedis stores the health check result in Redis
func (hc *Checker) updateHealthStatusInRedis(check Check) error {
	ctx := context.Background()
	data, err := json.Marshal(check)
	if err != nil {
		return err
	}
	return hc.redisClient.(interface {
		Set(ctx context.Context, key string, value interface{}, expiration time.Duration) error
	}).Set(ctx, check.EndpointURL, data, 0)
}

// GetHealthStatus retrieves the health status of an endpoint from Redis
func (hc *Checker) GetHealthStatus(endpointURL string) (Check, error) {
	ctx := context.Background()
	data, err := hc.redisClient.(interface {
		Get(ctx context.Context, key string) (string, error)
	}).Get(ctx, endpointURL)
	if err != nil {
		return Check{}, err
	}

	var check Check
	err = json.Unmarshal([]byte(data), &check)
	return check, err
}
