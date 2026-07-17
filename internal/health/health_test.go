package health

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"aetherlay/internal/store"
)

func TestNewChecker(t *testing.T) {
	valkeyClient := store.NewMockValkeyClient()
	checker := &Checker{
		valkeyClient: valkeyClient,
	}

	if checker.valkeyClient != valkeyClient {
		t.Error("Valkey client should be set correctly")
	}
}

func TestCheckHealthWithHealthyEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("healthy"))
	}))
	defer server.Close()

	valkeyClient := store.NewMockValkeyClient()
	checker := &Checker{
		valkeyClient: valkeyClient,
	}

	err := checker.CheckHealth(server.URL)
	if err != nil {
		t.Fatalf("Health check should succeed for healthy endpoint: %v", err)
	}

	healthCheck, err := checker.GetHealthStatus(server.URL)
	if err != nil {
		t.Fatalf("Failed to get health status from Valkey: %v", err)
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
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("unhealthy"))
	}))
	defer server.Close()

	valkeyClient := store.NewMockValkeyClient()
	checker := &Checker{
		valkeyClient: valkeyClient,
	}

	err := checker.CheckHealth(server.URL)
	if err != nil {
		t.Fatalf("Health check should succeed even for unhealthy endpoint: %v", err)
	}

	healthCheck, err := checker.GetHealthStatus(server.URL)
	if err != nil {
		t.Fatalf("Failed to get health status from Valkey: %v", err)
	}

	if healthCheck.HealthStatus {
		t.Error("Health status should be false for unhealthy endpoint")
	}
}

func TestCheckHealthWithNetworkError(t *testing.T) {
	valkeyClient := store.NewMockValkeyClient()
	checker := &Checker{
		valkeyClient: valkeyClient,
	}

	err := checker.CheckHealth("https://non-existent-domain-that-will-fail.com")
	if err == nil {
		t.Error("Health check should fail for non-existent domain")
	}
}

func TestCheckHealthWithInvalidURL(t *testing.T) {
	valkeyClient := store.NewMockValkeyClient()
	checker := &Checker{
		valkeyClient: valkeyClient,
	}

	err := checker.CheckHealth("invalid-url")
	if err == nil {
		t.Error("Health check should fail for invalid URL")
	}
}

func TestGetHealthStatusFromValkey(t *testing.T) {
	valkeyClient := store.NewMockValkeyClient()
	checker := &Checker{
		valkeyClient: valkeyClient,
	}

	_, err := checker.GetHealthStatus("https://non-existent-endpoint.com")
	if err == nil {
		t.Error("Should return error for non-existent health status")
	}
}

func TestCheckSerialization(t *testing.T) {
	healthCheck := Check{
		EndpointURL:  "https://example.com",
		HealthStatus: true,
		LastChecked:  time.Now(),
	}

	data, err := json.Marshal(healthCheck)
	if err != nil {
		t.Fatalf("Failed to marshal health check: %v", err)
	}

	var unmarshaled Check
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

func TestCheckHealthWithTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("healthy"))
	}))
	defer server.Close()

	valkeyClient := store.NewMockValkeyClient()
	checker := &Checker{
		valkeyClient: valkeyClient,
	}

	// The current CheckHealth implementation uses a 5s timeout, so this will not timeout.
	// Adjusting the test to expect no error.
	err := checker.CheckHealth(server.URL)
	if err != nil {
		t.Errorf("Health check should not error for slow endpoint within timeout: %v", err)
	}
}

// TestMakeRPCCallDetects429WithRetryAfter tests that makeRPCCall's 429 handling parses a
// Retry-After header and forwards it (and chain/endpointID/protocol) to HandleRateLimitFunc.
func TestMakeRPCCallDetects429WithRetryAfter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "9")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	var gotChain, gotEndpointID, gotProtocol string
	var gotSignal RateLimitSignal
	checker := &Checker{
		valkeyClient: store.NewMockValkeyClient(),
		HandleRateLimitFunc: func(chain, endpointID, protocol string, signal RateLimitSignal) {
			gotChain, gotEndpointID, gotProtocol, gotSignal = chain, endpointID, protocol, signal
		},
	}

	_, err := checker.makeRPCCall(context.Background(), server.URL, "eth_blockNumber", "ethereum", "ep1", "alchemy")
	if err == nil {
		t.Fatal("Expected error from 429 response")
	}

	if gotChain != "ethereum" || gotEndpointID != "ep1" || gotProtocol != "http" {
		t.Errorf("HandleRateLimitFunc called with unexpected args: chain=%s endpointID=%s protocol=%s", gotChain, gotEndpointID, gotProtocol)
	}
	if !gotSignal.IsRateLimited {
		t.Error("Expected IsRateLimited to be true")
	}
	if gotSignal.RetryAfter != 9*time.Second {
		t.Errorf("Expected RetryAfter to be 9s, got %v", gotSignal.RetryAfter)
	}
}

// TestMakeRPCCallDetects402DailyCapForInfura tests that a 402 from an Infura endpoint is
// classified as a daily-quota rate-limit signal.
func TestMakeRPCCallDetects402DailyCapForInfura(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
	}))
	defer server.Close()

	var gotSignal RateLimitSignal
	called := false
	checker := &Checker{
		valkeyClient: store.NewMockValkeyClient(),
		HandleRateLimitFunc: func(chain, endpointID, protocol string, signal RateLimitSignal) {
			called = true
			gotSignal = signal
		},
	}

	_, err := checker.makeRPCCall(context.Background(), server.URL, "eth_blockNumber", "ethereum", "ep1", "infura")
	if err == nil {
		t.Fatal("Expected error from 402 response")
	}
	if !called {
		t.Fatal("Expected HandleRateLimitFunc to be called")
	}
	if !gotSignal.IsDailyQuota {
		t.Error("Expected IsDailyQuota to be true for an Infura 402")
	}
}

// TestMakeRPCCallDetectsEmbeddedRateLimitErrorIn200Response tests the periodic
// health-check path's version of the same blind spot closed on the live proxy path: a
// 200 response whose JSON-RPC body carries a rate-limit error code.
func TestMakeRPCCallDetectsEmbeddedRateLimitErrorIn200Response(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32005,"message":"Request limit exceeded"}}`))
	}))
	defer server.Close()

	called := false
	checker := &Checker{
		valkeyClient: store.NewMockValkeyClient(),
		HandleRateLimitFunc: func(chain, endpointID, protocol string, signal RateLimitSignal) {
			called = true
		},
	}

	_, err := checker.makeRPCCall(context.Background(), server.URL, "eth_blockNumber", "ethereum", "ep1", "alchemy")
	if err == nil {
		t.Fatal("Expected an error for the embedded JSON-RPC error")
	}
	if !called {
		t.Error("Expected HandleRateLimitFunc to be called for a 200 response with an embedded rate-limit error")
	}
}

// TestMakeRPCCallSuccessPathUnaffected is a regression guard: a clean success response
// must not trigger HandleRateLimitFunc and must still return the parsed result.
func TestMakeRPCCallSuccessPathUnaffected(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x123"}`))
	}))
	defer server.Close()

	called := false
	checker := &Checker{
		valkeyClient: store.NewMockValkeyClient(),
		HandleRateLimitFunc: func(chain, endpointID, protocol string, signal RateLimitSignal) {
			called = true
		},
	}

	result, err := checker.makeRPCCall(context.Background(), server.URL, "eth_blockNumber", "ethereum", "ep1", "alchemy")
	if err != nil {
		t.Fatalf("Expected no error for a clean success response, got %v", err)
	}
	if called {
		t.Error("Expected HandleRateLimitFunc NOT to be called for a clean success response")
	}
	if result != "0x123" {
		t.Errorf("Expected result '0x123', got %v", result)
	}
}

func TestStartEphemeralChecksDisabled(t *testing.T) {
	checker := &Checker{
		ephemeralChecksEnabled: false,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// StartEphemeralChecks should return immediately when disabled.
	// If it blocks (enters the ticker loop), the test will hang and timeout.
	done := make(chan struct{})
	go func() {
		checker.StartEphemeralChecks(ctx)
		close(done)
	}()

	select {
	case <-done:
		// Success: returned immediately
	case <-time.After(2 * time.Second):
		t.Fatal("StartEphemeralChecks did not return immediately when disabled")
	}
}
