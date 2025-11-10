package cache

import (
	"testing"
	"time"

	"aetherlay/internal/store"
)

func TestNewHealthCache(t *testing.T) {
	ttl := 10 * time.Second
	cache := NewHealthCache(ttl)

	if cache == nil {
		t.Fatal("NewHealthCache returned nil")
	}
	if cache.ttl != ttl {
		t.Errorf("Expected TTL %v, got %v", ttl, cache.ttl)
	}
	if cache.entries == nil {
		t.Error("Cache entries map not initialized")
	}
}

func TestHealthCache_GetAndSet(t *testing.T) {
	cache := NewHealthCache(10 * time.Second)
	chain := "ethereum"
	endpoint := "test-endpoint"

	// Initially, cache should be empty
	status, found := cache.Get(chain, endpoint)
	if found {
		t.Error("Expected cache miss for new entry")
	}
	if status != nil {
		t.Error("Expected nil status on cache miss")
	}

	// Set a value
	testStatus := &store.EndpointStatus{
		HealthyHTTP:  true,
		HealthyWS:    false,
		HasHTTP:      true,
		HasWS:        false,
		BlockNumber:  12345,
		Requests24h:  100,
		Version:      1,
	}
	cache.Set(chain, endpoint, testStatus)

	// Get the value back
	status, found = cache.Get(chain, endpoint)
	if !found {
		t.Fatal("Expected cache hit after Set")
	}
	if status == nil {
		t.Fatal("Expected non-nil status on cache hit")
	}
	if status.HealthyHTTP != testStatus.HealthyHTTP {
		t.Errorf("Expected HealthyHTTP=%v, got %v", testStatus.HealthyHTTP, status.HealthyHTTP)
	}
	if status.BlockNumber != testStatus.BlockNumber {
		t.Errorf("Expected BlockNumber=%d, got %d", testStatus.BlockNumber, status.BlockNumber)
	}
}

func TestHealthCache_TTL(t *testing.T) {
	ttl := 100 * time.Millisecond
	cache := NewHealthCache(ttl)
	chain := "ethereum"
	endpoint := "test-endpoint"

	testStatus := &store.EndpointStatus{
		HealthyHTTP: true,
		HasHTTP:     true,
		BlockNumber: 12345,
		Version:     1,
	}
	cache.Set(chain, endpoint, testStatus)

	// Should be available immediately
	status, found := cache.Get(chain, endpoint)
	if !found {
		t.Fatal("Expected cache hit immediately after Set")
	}
	if status.BlockNumber != 12345 {
		t.Errorf("Expected BlockNumber=12345, got %d", status.BlockNumber)
	}

	// Wait for TTL to expire
	time.Sleep(150 * time.Millisecond)

	// Should be expired now
	status, found = cache.Get(chain, endpoint)
	if found {
		t.Error("Expected cache miss after TTL expiration")
	}
	if status != nil {
		t.Error("Expected nil status after TTL expiration")
	}
}

func TestHealthCache_Invalidate(t *testing.T) {
	cache := NewHealthCache(10 * time.Second)
	chain := "ethereum"
	endpoint := "test-endpoint"

	testStatus := &store.EndpointStatus{
		HealthyHTTP: true,
		HasHTTP:     true,
		Version:     1,
	}
	cache.Set(chain, endpoint, testStatus)

	// Verify it's in cache
	_, found := cache.Get(chain, endpoint)
	if !found {
		t.Fatal("Expected cache hit after Set")
	}

	// Invalidate
	cache.Invalidate(chain, endpoint)

	// Verify it's gone
	status, found := cache.Get(chain, endpoint)
	if found {
		t.Error("Expected cache miss after Invalidate")
	}
	if status != nil {
		t.Error("Expected nil status after Invalidate")
	}
}

func TestHealthCache_Clear(t *testing.T) {
	cache := NewHealthCache(10 * time.Second)

	testStatus := &store.EndpointStatus{
		HealthyHTTP: true,
		HasHTTP:     true,
		Version:     1,
	}

	// Set multiple entries
	cache.Set("ethereum", "endpoint1", testStatus)
	cache.Set("ethereum", "endpoint2", testStatus)
	cache.Set("polygon", "endpoint1", testStatus)

	// Verify they're all in cache
	if _, found := cache.Get("ethereum", "endpoint1"); !found {
		t.Error("Expected cache hit for ethereum:endpoint1")
	}
	if _, found := cache.Get("ethereum", "endpoint2"); !found {
		t.Error("Expected cache hit for ethereum:endpoint2")
	}
	if _, found := cache.Get("polygon", "endpoint1"); !found {
		t.Error("Expected cache hit for polygon:endpoint1")
	}

	// Clear all
	cache.Clear()

	// Verify they're all gone
	if _, found := cache.Get("ethereum", "endpoint1"); found {
		t.Error("Expected cache miss after Clear for ethereum:endpoint1")
	}
	if _, found := cache.Get("ethereum", "endpoint2"); found {
		t.Error("Expected cache miss after Clear for ethereum:endpoint2")
	}
	if _, found := cache.Get("polygon", "endpoint1"); found {
		t.Error("Expected cache miss after Clear for polygon:endpoint1")
	}
}

func TestHealthCache_MultipleChains(t *testing.T) {
	cache := NewHealthCache(10 * time.Second)

	status1 := &store.EndpointStatus{
		HealthyHTTP: true,
		HasHTTP:     true,
		BlockNumber: 1000,
		Version:     1,
	}
	status2 := &store.EndpointStatus{
		HealthyHTTP: false,
		HasHTTP:     true,
		BlockNumber: 2000,
		Version:     1,
	}

	// Set different statuses for different chains
	cache.Set("ethereum", "endpoint1", status1)
	cache.Set("polygon", "endpoint1", status2)

	// Get and verify they're independent
	ethStatus, found := cache.Get("ethereum", "endpoint1")
	if !found {
		t.Fatal("Expected cache hit for ethereum:endpoint1")
	}
	if ethStatus.BlockNumber != 1000 {
		t.Errorf("Expected BlockNumber=1000 for ethereum, got %d", ethStatus.BlockNumber)
	}

	polyStatus, found := cache.Get("polygon", "endpoint1")
	if !found {
		t.Fatal("Expected cache hit for polygon:endpoint1")
	}
	if polyStatus.BlockNumber != 2000 {
		t.Errorf("Expected BlockNumber=2000 for polygon, got %d", polyStatus.BlockNumber)
	}
}

func TestHealthCache_ConcurrentAccess(t *testing.T) {
	cache := NewHealthCache(10 * time.Second)
	chain := "ethereum"
	endpoint := "test-endpoint"

	testStatus := &store.EndpointStatus{
		HealthyHTTP: true,
		HasHTTP:     true,
		BlockNumber: 12345,
		Version:     1,
	}

	done := make(chan bool)

	// Concurrent writers
	for i := 0; i < 10; i++ {
		go func(blockNum int64) {
			status := &store.EndpointStatus{
				HealthyHTTP: true,
				HasHTTP:     true,
				BlockNumber: blockNum,
				Version:     1,
			}
			cache.Set(chain, endpoint, status)
			done <- true
		}(int64(i))
	}

	// Concurrent readers
	for i := 0; i < 10; i++ {
		go func() {
			_, _ = cache.Get(chain, endpoint)
			done <- true
		}()
	}

	// Wait for all goroutines
	for i := 0; i < 20; i++ {
		<-done
	}

	// Verify cache still works after concurrent access
	cache.Set(chain, endpoint, testStatus)
	status, found := cache.Get(chain, endpoint)
	if !found {
		t.Error("Cache broken after concurrent access")
	}
	if status.BlockNumber != 12345 {
		t.Errorf("Expected BlockNumber=12345, got %d", status.BlockNumber)
	}
}
