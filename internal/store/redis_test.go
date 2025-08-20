package store

import (
	"aetherlay/internal/helpers"
	"context"
	"fmt"
	"net"
	"os"
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

// TestNewRedisClientTLSConfig is an integration test that checks the TLS configuration.
// It requires a running Redis server with TLS enabled on port 6380 and non-TLS on 6379.
func TestNewRedisClientTLSConfig(t *testing.T) {
	redisHost := helpers.GetStringFromFlagOrEnv("redis-host", "REDIS_HOST", "localhost")
	redisPass := helpers.GetStringFromFlagOrEnv("redis-pass", "REDIS_PASS", "SOME_PASS")

	redisAddrNonTLS := fmt.Sprintf("%s:6379", redisHost)
	redisAddrTLS := fmt.Sprintf("%s:6380", redisHost)

	// Pre-flight check to see if Redis is available. If not, skip the test.
	conn, err := net.DialTimeout("tcp", redisAddrNonTLS, 1*time.Second)
	if err != nil {
		t.Skipf("Skipping integration test: Redis is not available at %s. Error: %v", redisAddrNonTLS, err)
	}
	conn.Close()

	ctx := context.Background()

	t.Run("Non-TLS connection", func(t *testing.T) {
		client := NewRedisClient(redisAddrNonTLS, redisPass, false, false)
		if err := client.Ping(ctx); err != nil {
			t.Fatalf("Failed to connect to non-TLS Redis: %v", err)
		}
	})

	t.Run("TLS connection with skip verify", func(t *testing.T) {
		os.Setenv("REDIS_SKIP_TLS_CHECK", "true")
		defer os.Unsetenv("REDIS_SKIP_TLS_CHECK")

		client := NewRedisClient(redisAddrTLS, redisPass, true, true)
		if err := client.Ping(ctx); err != nil {
			t.Fatalf("Failed to connect to TLS Redis with skip verify: %v", err)
		}
	})

	t.Run("TLS connection without skip verify (should fail)", func(t *testing.T) {
		os.Setenv("REDIS_SKIP_TLS_CHECK", "false")
		defer os.Unsetenv("REDIS_SKIP_TLS_CHECK")

		client := NewRedisClient(redisAddrTLS, redisPass, true, true)
		if err := client.Ping(ctx); err == nil {
			t.Fatal("Expected TLS connection to fail without skip verify, but it succeeded.")
		} else {
			t.Logf("Received expected error: %v", err)
		}
	})

	t.Run("TLS connection with env var unset (should fail)", func(t *testing.T) {
		os.Unsetenv("REDIS_SKIP_TLS_CHECK")

		client := NewRedisClient(redisAddrTLS, redisPass, true, true)
		if err := client.Ping(ctx); err == nil {
			t.Fatal("Expected TLS connection to fail with env var unset, but it succeeded.")
		} else {
			t.Logf("Received expected error: %v", err)
		}
	})
}
