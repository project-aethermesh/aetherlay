package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"aetherlay/internal/config"
	"aetherlay/internal/store"
)

// TestGetAvailableEndpointsSkipsAtCapacityEndpoint tests that an endpoint at its
// configured capacity ceiling for the current window is excluded from selection,
// independent of the (unrelated) provider-triggered RateLimited state.
func TestGetAvailableEndpointsSkipsAtCapacityEndpoint(t *testing.T) {
	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"ethereum": {
				"under-capacity": config.Endpoint{
					Provider: "alchemy",
					Role:     "primary",
					Type:     "full",
					HTTPURL:  "http://under-capacity.com",
					Capacity: &config.CapacityLimit{MaxRequests: 5, WindowSeconds: 10},
				},
				"at-capacity": config.Endpoint{
					Provider: "alchemy",
					Role:     "primary",
					Type:     "full",
					HTTPURL:  "http://at-capacity.com",
					Capacity: &config.CapacityLimit{MaxRequests: 2, WindowSeconds: 10},
				},
			},
		},
	}
	valkeyClient := store.NewMockValkeyClient()
	valkeyClient.PopulateStatuses(map[string]*store.EndpointStatus{
		"ethereum:under-capacity": {HasHTTP: true, HealthyHTTP: true},
		"ethereum:at-capacity":    {HasHTTP: true, HealthyHTTP: true},
	})
	ctx := context.Background()
	valkeyClient.IncrementCapacityCount(ctx, "ethereum", "at-capacity", 10)
	valkeyClient.IncrementCapacityCount(ctx, "ethereum", "at-capacity", 10)

	appConfig := createTestConfig()
	appConfig.CapacityThrottlingEnabled = true
	server := NewServer(cfg, valkeyClient, appConfig)

	endpoints := server.getAvailableEndpoints("ethereum", false, false)

	if len(endpoints) != 1 {
		t.Fatalf("Expected 1 available endpoint, got %d", len(endpoints))
	}
	if endpoints[0].ID != "under-capacity" {
		t.Errorf("Expected under-capacity endpoint, got %s", endpoints[0].ID)
	}
}

// TestGetAvailableEndpointsCapacityKillSwitch tests that CapacityThrottlingEnabled=false
// fully bypasses the capacity gate, even for an endpoint at its configured ceiling.
func TestGetAvailableEndpointsCapacityKillSwitch(t *testing.T) {
	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"ethereum": {
				"at-capacity": config.Endpoint{
					Provider: "alchemy",
					Role:     "primary",
					Type:     "full",
					HTTPURL:  "http://at-capacity.com",
					Capacity: &config.CapacityLimit{MaxRequests: 2, WindowSeconds: 10},
				},
			},
		},
	}
	valkeyClient := store.NewMockValkeyClient()
	valkeyClient.PopulateStatuses(map[string]*store.EndpointStatus{
		"ethereum:at-capacity": {HasHTTP: true, HealthyHTTP: true},
	})
	ctx := context.Background()
	valkeyClient.IncrementCapacityCount(ctx, "ethereum", "at-capacity", 10)
	valkeyClient.IncrementCapacityCount(ctx, "ethereum", "at-capacity", 10)

	appConfig := createTestConfig()
	appConfig.CapacityThrottlingEnabled = false
	server := NewServer(cfg, valkeyClient, appConfig)

	endpoints := server.getAvailableEndpoints("ethereum", false, false)

	if len(endpoints) != 1 {
		t.Fatalf("Expected the at-capacity endpoint to still be available with the kill switch off, got %d endpoints", len(endpoints))
	}
}

// TestSelectBestEndpointByRoleFallsBackWhenNotAllHaveCapacity tests that selection falls
// back to raw 24h-count comparison (today's behavior) when not every candidate endpoint
// in the role has a configured Capacity - avoiding comparing endpoints on incompatible units.
func TestSelectBestEndpointByRoleFallsBackWhenNotAllHaveCapacity(t *testing.T) {
	valkeyClient := store.NewMockValkeyClient()
	ctx := context.Background()

	valkeyClient.IncrementRequestCount(ctx, "ethereum", "ep-a", "proxy_requests")
	for i := 0; i < 5; i++ {
		valkeyClient.IncrementRequestCount(ctx, "ethereum", "ep-b", "proxy_requests")
	}

	endpoints := []EndpointWithID{
		{ID: "ep-a", Endpoint: config.Endpoint{Role: "primary", Type: "full", HTTPURL: "http://a"}},
		{ID: "ep-b", Endpoint: config.Endpoint{
			Role: "primary", Type: "full", HTTPURL: "http://b",
			Capacity: &config.CapacityLimit{MaxRequests: 1000000, WindowSeconds: 1},
		}},
	}

	server := NewServer(&config.Config{}, valkeyClient, createTestConfig())
	best := server.selectBestEndpointByRole("ethereum", endpoints, "primary")

	if best == nil {
		t.Fatal("Expected a best endpoint")
	}
	if best.ID != "ep-a" {
		t.Errorf("Expected ep-a (lowest raw count, fallback behavior since not all candidates have Capacity), got %s", best.ID)
	}
}

// TestSelectBestEndpointByRoleWeightsByCapacityWhenAllConfigured tests that, once every
// candidate endpoint in the role has a configured Capacity, selection is weighted by
// utilization relative to each endpoint's own ceiling rather than raw request count.
func TestSelectBestEndpointByRoleWeightsByCapacityWhenAllConfigured(t *testing.T) {
	valkeyClient := store.NewMockValkeyClient()
	ctx := context.Background()

	for i := 0; i < 10; i++ {
		valkeyClient.IncrementRequestCount(ctx, "ethereum", "low-capacity", "proxy_requests")
		valkeyClient.IncrementRequestCount(ctx, "ethereum", "high-capacity", "proxy_requests")
	}

	endpoints := []EndpointWithID{
		{ID: "low-capacity", Endpoint: config.Endpoint{
			Role: "primary", Type: "full", HTTPURL: "http://low",
			Capacity: &config.CapacityLimit{MaxRequests: 10, WindowSeconds: 1},
		}},
		{ID: "high-capacity", Endpoint: config.Endpoint{
			Role: "primary", Type: "full", HTTPURL: "http://high",
			Capacity: &config.CapacityLimit{MaxRequests: 1000, WindowSeconds: 1},
		}},
	}

	server := NewServer(&config.Config{}, valkeyClient, createTestConfig())
	best := server.selectBestEndpointByRole("ethereum", endpoints, "primary")

	if best == nil {
		t.Fatal("Expected a best endpoint")
	}
	// Equal raw counts, but low-capacity's small ceiling means a much higher utilization
	// ratio than high-capacity's - so high-capacity should be preferred.
	if best.ID != "high-capacity" {
		t.Errorf("Expected high-capacity (lower utilization ratio despite equal raw count), got %s", best.ID)
	}
}

// TestRecordCapacityUsageIncrementsRegardlessOfOutcome tests that recordCapacityUsage
// increments the counter on every call - the caller is responsible for calling it on
// every dispatch attempt whether it succeeds or fails, since a rejected attempt still
// spends a real unit of the provider's quota.
func TestRecordCapacityUsageIncrementsRegardlessOfOutcome(t *testing.T) {
	valkeyClient := store.NewMockValkeyClient()
	appConfig := createTestConfig()
	appConfig.CapacityThrottlingEnabled = true
	server := NewServer(&config.Config{}, valkeyClient, appConfig)

	ep := config.Endpoint{Capacity: &config.CapacityLimit{MaxRequests: 100, WindowSeconds: 10}}
	server.recordCapacityUsage("ethereum", ep, "ep1")
	server.recordCapacityUsage("ethereum", ep, "ep1")

	count, err := valkeyClient.GetCapacityCount(context.Background(), "ethereum", "ep1", 10)
	if err != nil {
		t.Fatalf("GetCapacityCount failed: %v", err)
	}
	if count != 2 {
		t.Errorf("Expected capacity count 2, got %d", count)
	}
}

// TestRecordCapacityUsageNoop tests the two conditions under which recordCapacityUsage
// must be a no-op: no Capacity configured, and the CapacityThrottlingEnabled kill switch.
func TestRecordCapacityUsageNoop(t *testing.T) {
	t.Run("no Capacity configured", func(t *testing.T) {
		valkeyClient := store.NewMockValkeyClient()
		appConfig := createTestConfig()
		appConfig.CapacityThrottlingEnabled = true
		server := NewServer(&config.Config{}, valkeyClient, appConfig)

		server.recordCapacityUsage("ethereum", config.Endpoint{}, "no-cap")

		count, _ := valkeyClient.GetCapacityCount(context.Background(), "ethereum", "no-cap", 10)
		if count != 0 {
			t.Errorf("Expected no increment without Capacity configured, got %d", count)
		}
	})

	t.Run("kill switch disabled", func(t *testing.T) {
		valkeyClient := store.NewMockValkeyClient()
		appConfig := createTestConfig()
		appConfig.CapacityThrottlingEnabled = false
		server := NewServer(&config.Config{}, valkeyClient, appConfig)

		ep := config.Endpoint{Capacity: &config.CapacityLimit{MaxRequests: 100, WindowSeconds: 10}}
		server.recordCapacityUsage("ethereum", ep, "disabled-ep")

		count, _ := valkeyClient.GetCapacityCount(context.Background(), "ethereum", "disabled-ep", 10)
		if count != 0 {
			t.Errorf("Expected no increment when CapacityThrottlingEnabled is false, got %d", count)
		}
	})
}

// TestHandleRequestHTTPRecordsCapacityUsagePerAttempt tests that a full HTTP request
// dispatched through the router increments the endpoint's capacity counter, confirming
// the dispatch-point wiring (not just the helper function in isolation).
func TestHandleRequestHTTPRecordsCapacityUsagePerAttempt(t *testing.T) {
	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"ethereum": {
				"ep1": config.Endpoint{
					Provider: "alchemy", Role: "primary", Type: "full", HTTPURL: "http://ep1",
					Capacity: &config.CapacityLimit{MaxRequests: 100, WindowSeconds: 10},
				},
			},
		},
	}
	valkeyClient := store.NewMockValkeyClient()
	valkeyClient.PopulateStatuses(map[string]*store.EndpointStatus{
		"ethereum:ep1": {HasHTTP: true, HealthyHTTP: true},
	})
	appConfig := createTestConfig()
	appConfig.CapacityThrottlingEnabled = true
	server := NewServer(cfg, valkeyClient, appConfig)
	server.forwardRequestWithBody = stubForwardRequestWithBody

	req := httptest.NewRequest("POST", "/ethereum", nil)
	w := httptest.NewRecorder()
	server.router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200 from stubbed forward, got %d", w.Code)
	}

	count, err := valkeyClient.GetCapacityCount(context.Background(), "ethereum", "ep1", 10)
	if err != nil {
		t.Fatalf("GetCapacityCount failed: %v", err)
	}
	if count != 1 {
		t.Errorf("Expected capacity count 1 after one successful dispatch, got %d", count)
	}
}
