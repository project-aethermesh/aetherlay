package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"aetherlay/internal/config"
	"aetherlay/internal/store"
)

// jsonRPCSuccessBody is a minimal, valid JSON-RPC success response body used by the
// "healthy" backends in the failover soak tests below.
const jsonRPCSuccessBody = `{"jsonrpc":"2.0","id":1,"result":"0x1"}`

// alwaysRateLimitedBackend returns a real httptest.Server that always responds with a
// 429, and a pointer to a counter tracking how many times it was actually dispatched to.
func alwaysRateLimitedBackend(t *testing.T) (*httptest.Server, *int64) {
	t.Helper()
	var calls int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(server.Close)
	return server, &calls
}

// alwaysErroringBackend returns a real httptest.Server that always responds with a
// generic 500 (the "or something else" non-rate-limit failure mode), and a call counter.
func alwaysErroringBackend(t *testing.T) (*httptest.Server, *int64) {
	t.Helper()
	var calls int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	return server, &calls
}

// alwaysHealthyBackend returns a real httptest.Server that always responds with a valid
// 200 JSON-RPC success body, and a call counter.
func alwaysHealthyBackend(t *testing.T) (*httptest.Server, *int64) {
	t.Helper()
	var calls int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(jsonRPCSuccessBody))
	}))
	t.Cleanup(server.Close)
	return server, &calls
}

// postRequest fires one real POST request through the server's actual router (the full
// retry loop, endpoint selection, and dispatch path - no stubbed forwarder).
func postRequest(server *Server, chain string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/"+chain, strings.NewReader(`{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}`))
	w := httptest.NewRecorder()
	server.router.ServeHTTP(w, req)
	return w
}

// TestManyRequestsAllServedWhenOneEndpointAlwaysRateLimited fires many requests at a
// two-endpoint chain where one endpoint always returns 429. Every single client-facing
// request must still succeed - the load balancer must fail over to the healthy endpoint,
// both via the internal per-request retry (for whichever request first tries the bad
// endpoint) and by excluding it globally afterward. Neither endpoint has a static
// `capacity` configured, so this also exercises adaptive capacity learning bootstrapping
// for the endpoint that gets rate limited.
func TestManyRequestsAllServedWhenOneEndpointAlwaysRateLimited(t *testing.T) {
	const rounds = 100

	flakyServer, flakyCalls := alwaysRateLimitedBackend(t)
	reliableServer, reliableCalls := alwaysHealthyBackend(t)

	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"ethereum": {
				"flaky":    config.Endpoint{Provider: "alchemy", Role: "primary", Type: "full", HTTPURL: flakyServer.URL},
				"reliable": config.Endpoint{Provider: "drpc", Role: "primary", Type: "full", HTTPURL: reliableServer.URL},
			},
		},
	}
	valkeyClient := store.NewMockValkeyClient()
	valkeyClient.PopulateStatuses(map[string]*store.EndpointStatus{
		"ethereum:flaky":    {HasHTTP: true, HealthyHTTP: true},
		"ethereum:reliable": {HasHTTP: true, HealthyHTTP: true},
	})

	appConfig := createTestConfig()
	appConfig.CapacityThrottlingEnabled = true
	appConfig.CapacityLearningEnabled = true
	server := NewServer(cfg, valkeyClient, appConfig)

	for i := 0; i < rounds; i++ {
		w := postRequest(server, "ethereum")
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d (body: %s)", i, w.Code, w.Body.String())
		}
	}

	if atomic.LoadInt64(flakyCalls) < 1 {
		t.Error("Expected the rate-limited endpoint to have been dispatched to at least once, to prove real failover (not just never selected)")
	}
	if got := atomic.LoadInt64(reliableCalls); got != rounds {
		t.Errorf("Expected the reliable endpoint to have served all %d successful responses, got %d", rounds, got)
	}

	state, err := valkeyClient.GetRateLimitState(context.Background(), "ethereum", "flaky")
	if err != nil {
		t.Fatalf("GetRateLimitState failed: %v", err)
	}
	if !state.RateLimited {
		t.Error("Expected the flaky endpoint to end up marked as rate limited")
	}

	estimate, err := valkeyClient.GetCapacityEstimate(context.Background(), "ethereum", "flaky")
	if err != nil {
		t.Fatalf("GetCapacityEstimate failed: %v", err)
	}
	if !estimate.HasEstimate {
		t.Error("Expected an adaptive capacity estimate to have been learned for the rate-limited endpoint")
	}
}

// TestManyRequestsAllServedWhenOneEndpointHasGenericErrors covers the "or something
// else" failure mode: a plain 500, which excludes an endpoint via the generic
// failure-threshold path rather than rate-limit detection. Every request must still succeed.
func TestManyRequestsAllServedWhenOneEndpointHasGenericErrors(t *testing.T) {
	const rounds = 100

	brokenServer, brokenCalls := alwaysErroringBackend(t)
	reliableServer, reliableCalls := alwaysHealthyBackend(t)

	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"ethereum": {
				"broken":   config.Endpoint{Provider: "alchemy", Role: "primary", Type: "full", HTTPURL: brokenServer.URL},
				"reliable": config.Endpoint{Provider: "drpc", Role: "primary", Type: "full", HTTPURL: reliableServer.URL},
			},
		},
	}
	valkeyClient := store.NewMockValkeyClient()
	valkeyClient.PopulateStatuses(map[string]*store.EndpointStatus{
		"ethereum:broken":   {HasHTTP: true, HealthyHTTP: true},
		"ethereum:reliable": {HasHTTP: true, HealthyHTTP: true},
	})

	server := NewServer(cfg, valkeyClient, createTestConfig())

	for i := 0; i < rounds; i++ {
		w := postRequest(server, "ethereum")
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d (body: %s)", i, w.Code, w.Body.String())
		}
	}

	if atomic.LoadInt64(brokenCalls) < 1 {
		t.Error("Expected the broken endpoint to have been dispatched to at least once, to prove real failover")
	}
	if got := atomic.LoadInt64(reliableCalls); got != rounds {
		t.Errorf("Expected the reliable endpoint to have served all %d successful responses, got %d", rounds, got)
	}

	status, err := valkeyClient.GetEndpointStatus(context.Background(), "ethereum", "broken")
	if err != nil {
		t.Fatalf("GetEndpointStatus failed: %v", err)
	}
	if status.HealthyHTTP {
		t.Error("Expected the broken endpoint to end up marked unhealthy")
	}
}

// TestManyRequestsAllServedWithMixedSimultaneousFailureModes combines a rate-limited
// endpoint, a generically-erroring endpoint, and a healthy one in the same chain. Every
// request must still succeed despite two of the three providers misbehaving at once.
func TestManyRequestsAllServedWithMixedSimultaneousFailureModes(t *testing.T) {
	const rounds = 150

	rateLimitedServer, _ := alwaysRateLimitedBackend(t)
	brokenServer, _ := alwaysErroringBackend(t)
	reliableServer, reliableCalls := alwaysHealthyBackend(t)

	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"ethereum": {
				"rate-limited": config.Endpoint{Provider: "alchemy", Role: "primary", Type: "full", HTTPURL: rateLimitedServer.URL},
				"broken":       config.Endpoint{Provider: "infura", Role: "primary", Type: "full", HTTPURL: brokenServer.URL},
				"reliable":     config.Endpoint{Provider: "drpc", Role: "primary", Type: "full", HTTPURL: reliableServer.URL},
			},
		},
	}
	valkeyClient := store.NewMockValkeyClient()
	valkeyClient.PopulateStatuses(map[string]*store.EndpointStatus{
		"ethereum:rate-limited": {HasHTTP: true, HealthyHTTP: true},
		"ethereum:broken":       {HasHTTP: true, HealthyHTTP: true},
		"ethereum:reliable":     {HasHTTP: true, HealthyHTTP: true},
	})

	appConfig := createTestConfig()
	appConfig.ProxyMaxRetries = 3 // must cover up to 2 failing endpoints before reaching the healthy one
	server := NewServer(cfg, valkeyClient, appConfig)

	for i := 0; i < rounds; i++ {
		w := postRequest(server, "ethereum")
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d (body: %s)", i, w.Code, w.Body.String())
		}
	}

	if got := atomic.LoadInt64(reliableCalls); got != rounds {
		t.Errorf("Expected the reliable endpoint to have served all %d successful responses, got %d", rounds, got)
	}

	rlState, _ := valkeyClient.GetRateLimitState(context.Background(), "ethereum", "rate-limited")
	if !rlState.RateLimited {
		t.Error("Expected the rate-limited endpoint to end up marked as rate limited")
	}
	brokenStatus, _ := valkeyClient.GetEndpointStatus(context.Background(), "ethereum", "broken")
	if brokenStatus.HealthyHTTP {
		t.Error("Expected the broken endpoint to end up marked unhealthy")
	}
}

// TestManyRequestsFailoverWhenStaticCapacityExhausted confirms the proactive
// static-capacity gate (no provider error involved at all) also fails over correctly:
// once a configured endpoint hits its self-imposed ceiling, later requests route to the
// other endpoint, and every request still succeeds.
func TestManyRequestsFailoverWhenStaticCapacityExhausted(t *testing.T) {
	const rounds = 50
	const ceilingLimit = 5

	cappedServer, cappedCalls := alwaysHealthyBackend(t)
	reliableServer, reliableCalls := alwaysHealthyBackend(t)

	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"ethereum": {
				"capped": config.Endpoint{
					Provider: "alchemy", Role: "primary", Type: "full", HTTPURL: cappedServer.URL,
					Capacity: &config.CapacityLimit{MaxRequests: ceilingLimit, WindowSeconds: 60},
				},
				"reliable": config.Endpoint{Provider: "drpc", Role: "primary", Type: "full", HTTPURL: reliableServer.URL},
			},
		},
	}
	valkeyClient := store.NewMockValkeyClient()
	valkeyClient.PopulateStatuses(map[string]*store.EndpointStatus{
		"ethereum:capped":   {HasHTTP: true, HealthyHTTP: true},
		"ethereum:reliable": {HasHTTP: true, HealthyHTTP: true},
	})

	appConfig := createTestConfig()
	appConfig.CapacityThrottlingEnabled = true
	server := NewServer(cfg, valkeyClient, appConfig)

	for i := 0; i < rounds; i++ {
		w := postRequest(server, "ethereum")
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d (body: %s)", i, w.Code, w.Body.String())
		}
	}

	if got := atomic.LoadInt64(cappedCalls); got != ceilingLimit {
		t.Errorf("Expected the capped endpoint to be dispatched to exactly %d times (its configured ceiling), got %d", ceilingLimit, got)
	}
	if got := atomic.LoadInt64(reliableCalls); got != rounds-ceilingLimit {
		t.Errorf("Expected the reliable endpoint to serve the remaining %d requests, got %d", rounds-ceilingLimit, got)
	}
}
