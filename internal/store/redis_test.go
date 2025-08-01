package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestNewRedisClient(t *testing.T) {
	client := NewMockRedisClient()
	if client == nil {
		t.Fatal("Redis client should not be nil")
	}
}

func TestNewEndpointStatus(t *testing.T) {
	status := NewEndpointStatus()

	if status.HasHTTP {
		t.Error("Default HasHTTP should be false")
	}
	if status.HasWS {
		t.Error("Default HasWS should be false")
	}
	if status.HealthyHTTP {
		t.Error("Default HealthyHTTP should be false")
	}
	if status.HealthyWS {
		t.Error("Default HealthyWS should be false")
	}
	if status.Requests24h != 0 {
		t.Error("Default 24h requests should be 0")
	}
	if status.Requests1Month != 0 {
		t.Error("Default 1 month requests should be 0")
	}
	if status.RequestsLifetime != 0 {
		t.Error("Default lifetime requests should be 0")
	}
}

func TestUpdateAndGetEndpointStatus(t *testing.T) {
	client := NewMockRedisClient()

	ctx := context.Background()
	chain := "test-chain"
	endpoint := "https://test.example.com"

	// Create a test status
	status := EndpointStatus{
		LastHealthCheck:  time.Now(),
		Requests24h:      10,
		Requests1Month:   100,
		RequestsLifetime: 1000,
		HasHTTP:          true,
		HasWS:            true,
		HealthyHTTP:      true,
		HealthyWS:        false,
	}

	// Update the status
	err := client.UpdateEndpointStatus(ctx, chain, endpoint, status)
	if err != nil {
		t.Fatalf("Update failed: %v", err)
	}

	// Get the status back
	retrievedStatus, err := client.GetEndpointStatus(ctx, chain, endpoint)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}

	if retrievedStatus.HasHTTP != status.HasHTTP {
		t.Errorf("Expected HasHTTP %t, got %t", status.HasHTTP, retrievedStatus.HasHTTP)
	}
	if retrievedStatus.HasWS != status.HasWS {
		t.Errorf("Expected HasWS %t, got %t", status.HasWS, retrievedStatus.HasWS)
	}
	if retrievedStatus.HealthyHTTP != status.HealthyHTTP {
		t.Errorf("Expected HealthyHTTP %t, got %t", status.HealthyHTTP, retrievedStatus.HealthyHTTP)
	}
	if retrievedStatus.HealthyWS != status.HealthyWS {
		t.Errorf("Expected HealthyWS %t, got %t", status.HealthyWS, retrievedStatus.HealthyWS)
	}
	if retrievedStatus.Requests24h != status.Requests24h {
		t.Errorf("Expected 24h requests %d, got %d", status.Requests24h, retrievedStatus.Requests24h)
	}
	if retrievedStatus.Requests1Month != status.Requests1Month {
		t.Errorf("Expected 1 month requests %d, got %d", status.Requests1Month, retrievedStatus.Requests1Month)
	}
	if retrievedStatus.RequestsLifetime != status.RequestsLifetime {
		t.Errorf("Expected lifetime requests %d, got %d", status.RequestsLifetime, retrievedStatus.RequestsLifetime)
	}
}

func TestGetEndpointStatusForNonExistentEndpoint(t *testing.T) {
	client := NewMockRedisClient()

	ctx := context.Background()
	chain := "test-chain"
	endpoint := "https://non-existent.example.com"

	// Get status for non-existent endpoint
	status, err := client.GetEndpointStatus(ctx, chain, endpoint)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}

	// Should return default status
	if status.HasHTTP {
		t.Error("Non-existent endpoint should have false HasHTTP")
	}
	if status.HasWS {
		t.Error("Non-existent endpoint should have false HasWS")
	}
	if status.HealthyHTTP {
		t.Error("Non-existent endpoint should have false HealthyHTTP")
	}
	if status.HealthyWS {
		t.Error("Non-existent endpoint should have false HealthyWS")
	}
	if status.Requests24h != 0 {
		t.Error("Non-existent endpoint should have 0 24h requests")
	}
	if status.Requests1Month != 0 {
		t.Error("Non-existent endpoint should have 0 1 month requests")
	}
	if status.RequestsLifetime != 0 {
		t.Error("Non-existent endpoint should have 0 lifetime requests")
	}
}

func uniqueTestKey(base string) string {
	return fmt.Sprintf("%s-%d", base, time.Now().UnixNano())
}

func cleanupTestKey(client *MockRedisClient, chain, endpoint string) {
	ctx := context.Background()
	// Remove all related keys
	prefix := fmt.Sprintf("metrics:%s:%s", chain, endpoint)
	client.Del(ctx, prefix+":proxy_requests:requests_24h")
	client.Del(ctx, prefix+":proxy_requests:requests_1m")
	client.Del(ctx, prefix+":proxy_requests:requests_all")
	client.Del(ctx, prefix+":health_requests:requests_24h")
	client.Del(ctx, prefix+":health_requests:requests_1m")
	client.Del(ctx, prefix+":health_requests:requests_all")
	client.Del(ctx, "health:"+chain+":"+endpoint)
}

func TestIncrementRequestCount(t *testing.T) {
	client := NewMockRedisClient()

	ctx := context.Background()
	chain := "test-chain"
	endpoint := uniqueTestKey("https://test.example.com")
	cleanupTestKey(client, chain, endpoint)
	defer cleanupTestKey(client, chain, endpoint)

	// Increment request count
	err := client.IncrementRequestCount(ctx, chain, endpoint, "proxy_requests")
	if err != nil {
		t.Fatalf("Increment failed: %v", err)
	}

	// Get the counts
	r24h, r1m, rAll, err := client.GetRequestCounts(ctx, chain, endpoint, "proxy_requests")
	if err != nil {
		t.Fatalf("Get counts failed: %v", err)
	}

	if r24h != 1 {
		t.Errorf("Expected 24h requests to be 1, got %d", r24h)
	}

	if r1m != 1 {
		t.Errorf("Expected 1 month requests to be 1, got %d", r1m)
	}

	if rAll != 1 {
		t.Errorf("Expected lifetime requests to be 1, got %d", rAll)
	}
}

func TestMultipleRequestCountIncrements(t *testing.T) {
	client := NewMockRedisClient()

	ctx := context.Background()
	chain := "test-chain"
	endpoint := uniqueTestKey("https://test.example.com")
	cleanupTestKey(client, chain, endpoint)
	defer cleanupTestKey(client, chain, endpoint)

	// Increment multiple times
	for i := 0; i < 5; i++ {
		err := client.IncrementRequestCount(ctx, chain, endpoint, "proxy_requests")
		if err != nil {
			t.Fatalf("Increment failed: %v", err)
		}
	}

	// Get the counts
	r24h, r1m, rAll, err := client.GetRequestCounts(ctx, chain, endpoint, "proxy_requests")
	if err != nil {
		t.Fatalf("Get counts failed: %v", err)
	}

	if r24h != 5 {
		t.Errorf("Expected 24h requests to be 5, got %d", r24h)
	}

	if r1m != 5 {
		t.Errorf("Expected 1 month requests to be 5, got %d", r1m)
	}

	if rAll != 5 {
		t.Errorf("Expected lifetime requests to be 5, got %d", rAll)
	}
}

func TestGetRequestCountsForNonExistentEndpoint(t *testing.T) {
	client := NewMockRedisClient()

	ctx := context.Background()
	chain := "test-chain"
	endpoint := "https://non-existent.example.com"

	// Get counts for non-existent endpoint
	r24h, r1m, rAll, err := client.GetRequestCounts(ctx, chain, endpoint, "proxy_requests")
	if err != nil {
		t.Fatalf("Get counts failed: %v", err)
	}

	if r24h != 0 {
		t.Errorf("Expected 24h requests to be 0, got %d", r24h)
	}

	if r1m != 0 {
		t.Errorf("Expected 1 month requests to be 0, got %d", r1m)
	}

	if rAll != 0 {
		t.Errorf("Expected lifetime requests to be 0, got %d", rAll)
	}
}

func TestCombinedRequestCounts(t *testing.T) {
	client := NewMockRedisClient()

	ctx := context.Background()
	chain := "test-chain"
	endpoint := uniqueTestKey("https://test.example.com")
	cleanupTestKey(client, chain, endpoint)
	defer cleanupTestKey(client, chain, endpoint)

	// Increment proxy request count
	err := client.IncrementRequestCount(ctx, chain, endpoint, "proxy_requests")
	if err != nil {
		t.Fatalf("Proxy increment failed: %v", err)
	}

	// Increment health request count
	err = client.IncrementRequestCount(ctx, chain, endpoint, "health_requests")
	if err != nil {
		t.Fatalf("Health increment failed: %v", err)
	}

	// Get combined counts
	r24h, r1m, rAll, err := client.GetCombinedRequestCounts(ctx, chain, endpoint)
	if err != nil {
		t.Fatalf("Get combined counts failed: %v", err)
	}

	if r24h != 2 {
		t.Errorf("Expected combined 24h requests to be 2, got %d", r24h)
	}

	if r1m != 2 {
		t.Errorf("Expected combined 1 month requests to be 2, got %d", r1m)
	}

	if rAll != 2 {
		t.Errorf("Expected combined lifetime requests to be 2, got %d", rAll)
	}
}
