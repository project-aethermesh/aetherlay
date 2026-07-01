package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aetherlay/internal/config"
	"aetherlay/internal/health"
	"aetherlay/internal/helpers"
	"aetherlay/internal/store"

	"github.com/gorilla/websocket"
)

// stubForwardRequestWithBody is a stub for HTTP forwarding with body in tests.
func stubForwardRequestWithBody(w http.ResponseWriter, ctx context.Context, method, targetURL string, bodyBytes []byte, headers http.Header) error {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("stubbed"))
	return nil
}

// stubProxyWebSocket is a stub for WebSocket proxying in tests.
func stubProxyWebSocket(w http.ResponseWriter, r *http.Request, backendURL string) error {
	w.WriteHeader(http.StatusSwitchingProtocols)
	return nil
}

// failingForwardRequestWithBody simulates a failing endpoint for testing retry logic with body.
func failingForwardRequestWithBody(w http.ResponseWriter, ctx context.Context, method, targetURL string, bodyBytes []byte, headers http.Header) error {
	return fmt.Errorf("endpoint failed: %s", targetURL)
}

// createTestConfig creates a default LoadedConfig for testing.
func createTestConfig() *helpers.LoadedConfig {
	return &helpers.LoadedConfig{
		EphemeralChecksEnabled: true,
		ProxyMaxRetries:        3,
		ProxyTimeout:           15,
		ProxyTimeoutPerTry:     5,
	}
}

// TestServerHealthCheck tests the /health endpoint handler.
func TestServerHealthCheck(t *testing.T) {
	cfg := &config.Config{}
	valkeyClient := store.NewMockValkeyClient()
	server := NewServer(cfg, valkeyClient, createTestConfig())

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()

	server.router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status code %d, got %d", http.StatusOK, w.Code)
	}
}

// TestHTTPSelection_HealthyOnly tests HTTP selection when only one endpoint is healthy.
func TestHTTPSelection_HealthyOnly(t *testing.T) {
	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"chainA": {
				"ep1": config.Endpoint{Provider: "ep1", HTTPURL: "http://a", WSURL: "ws://a", Role: "primary", Type: "full"},
				"ep2": config.Endpoint{Provider: "ep2", HTTPURL: "http://b", WSURL: "ws://b", Role: "primary", Type: "full"},
			},
		},
	}
	valkeyClient := store.NewMockValkeyClient()
	valkeyClient.PopulateStatuses(map[string]*store.EndpointStatus{
		"chainA:ep1": {HasHTTP: true, HealthyHTTP: true},
		"chainA:ep2": {HasHTTP: true, HealthyHTTP: false},
	})
	server := NewServer(cfg, valkeyClient, createTestConfig())
	server.forwardRequestWithBody = stubForwardRequestWithBody
	server.proxyWebSocket = stubProxyWebSocket

	req := httptest.NewRequest("POST", "/chainA", nil)
	w := httptest.NewRecorder()
	server.router.ServeHTTP(w, req)

	if w.Code == http.StatusServiceUnavailable {
		t.Errorf("Expected a healthy endpoint to be selected, got 503")
	}
}

// TestHTTPSelection_NoneHealthy tests HTTP selection when no endpoints are healthy.
func TestHTTPSelection_NoneHealthy(t *testing.T) {
	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"chainA": {
				"ep1": config.Endpoint{Provider: "ep1", HTTPURL: "http://a", Role: "primary", Type: "full"},
			},
		},
	}
	valkeyClient := store.NewMockValkeyClient()
	valkeyClient.PopulateStatuses(map[string]*store.EndpointStatus{
		"chainA:ep1": {HasHTTP: true, HealthyHTTP: false},
	})
	server := NewServer(cfg, valkeyClient, createTestConfig())
	server.forwardRequestWithBody = stubForwardRequestWithBody
	server.proxyWebSocket = stubProxyWebSocket

	req := httptest.NewRequest("POST", "/chainA", nil)
	w := httptest.NewRecorder()
	server.router.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("Expected 503 when no healthy HTTP endpoints, got %d", w.Code)
	}
}

// TestWSSelection_HealthyOnly tests WS selection when only one endpoint is healthy.
func TestWSSelection_HealthyOnly(t *testing.T) {
	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"chainB": {
				"ep1": config.Endpoint{Provider: "ep1", WSURL: "ws://a", Role: "primary", Type: "full"},
				"ep2": config.Endpoint{Provider: "ep2", WSURL: "ws://b", Role: "primary", Type: "full"},
			},
		},
	}
	valkeyClient := store.NewMockValkeyClient()
	valkeyClient.PopulateStatuses(map[string]*store.EndpointStatus{
		"chainB:ep1": {HasWS: true, HealthyWS: true},
		"chainB:ep2": {HasWS: true, HealthyWS: false},
	})
	server := NewServer(cfg, valkeyClient, createTestConfig())
	server.forwardRequestWithBody = stubForwardRequestWithBody
	server.proxyWebSocket = stubProxyWebSocket

	req := httptest.NewRequest("GET", "/chainB", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	w := httptest.NewRecorder()
	server.router.ServeHTTP(w, req)

	if w.Code == http.StatusServiceUnavailable {
		t.Errorf("Expected a healthy WS endpoint to be selected, got 503")
	}
}

// TestWSSelection_NoneHealthy tests WS selection when no endpoints are healthy.
func TestWSSelection_NoneHealthy(t *testing.T) {
	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"chainB": {
				"ep1": config.Endpoint{Provider: "ep1", WSURL: "ws://a", Role: "primary", Type: "full"},
			},
		},
	}
	valkeyClient := store.NewMockValkeyClient()
	valkeyClient.PopulateStatuses(map[string]*store.EndpointStatus{
		"chainB:ep1": {HasWS: true, HealthyWS: false},
	})
	server := NewServer(cfg, valkeyClient, createTestConfig())
	server.forwardRequestWithBody = stubForwardRequestWithBody
	server.proxyWebSocket = stubProxyWebSocket

	req := httptest.NewRequest("GET", "/chainB", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	w := httptest.NewRecorder()
	server.router.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("Expected 503 when no healthy WS endpoints, got %d", w.Code)
	}
}

// TestHTTPSelection_FallbackWhenPrimaryUnhealthy tests fallback logic for HTTP when primaries are unhealthy.
func TestHTTPSelection_FallbackWhenPrimaryUnhealthy(t *testing.T) {
	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"chainA": {
				"primary1":  config.Endpoint{Provider: "primary1", HTTPURL: "http://primary1", Role: "primary", Type: "full"},
				"primary2":  config.Endpoint{Provider: "primary2", HTTPURL: "http://primary2", Role: "primary", Type: "full"},
				"fallback1": config.Endpoint{Provider: "fallback1", HTTPURL: "http://fallback1", Role: "fallback", Type: "full"},
				"fallback2": config.Endpoint{Provider: "fallback2", HTTPURL: "http://fallback2", Role: "fallback", Type: "full"},
			},
		},
	}
	valkeyClient := store.NewMockValkeyClient()
	valkeyClient.PopulateStatuses(map[string]*store.EndpointStatus{
		"chainA:primary1":  {HasHTTP: true, HealthyHTTP: false},
		"chainA:primary2":  {HasHTTP: true, HealthyHTTP: false},
		"chainA:fallback1": {HasHTTP: true, HealthyHTTP: true},
		"chainA:fallback2": {HasHTTP: true, HealthyHTTP: true},
	})
	server := NewServer(cfg, valkeyClient, createTestConfig())
	server.forwardRequestWithBody = stubForwardRequestWithBody
	server.proxyWebSocket = stubProxyWebSocket

	req := httptest.NewRequest("POST", "/chainA", nil)
	w := httptest.NewRecorder()
	server.router.ServeHTTP(w, req)

	if w.Code == http.StatusServiceUnavailable {
		t.Errorf("Expected fallback endpoint to be selected when primaries are unhealthy, got 503")
	}
}

// TestWSSelection_FallbackWhenPrimaryUnhealthy tests fallback logic for WS when primaries are unhealthy.
func TestWSSelection_FallbackWhenPrimaryUnhealthy(t *testing.T) {
	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"chainB": {
				"primary1":  config.Endpoint{Provider: "primary1", WSURL: "ws://primary1", Role: "primary", Type: "full"},
				"primary2":  config.Endpoint{Provider: "primary2", WSURL: "ws://primary2", Role: "primary", Type: "full"},
				"fallback1": config.Endpoint{Provider: "fallback1", WSURL: "ws://fallback1", Role: "fallback", Type: "full"},
				"fallback2": config.Endpoint{Provider: "fallback2", WSURL: "ws://fallback2", Role: "fallback", Type: "full"},
			},
		},
	}
	valkeyClient := store.NewMockValkeyClient()
	valkeyClient.PopulateStatuses(map[string]*store.EndpointStatus{
		"chainB:primary1":  {HasWS: true, HealthyWS: false},
		"chainB:primary2":  {HasWS: true, HealthyWS: false},
		"chainB:fallback1": {HasWS: true, HealthyWS: true},
		"chainB:fallback2": {HasWS: true, HealthyWS: true},
	})
	server := NewServer(cfg, valkeyClient, createTestConfig())
	server.forwardRequestWithBody = stubForwardRequestWithBody
	server.proxyWebSocket = stubProxyWebSocket

	req := httptest.NewRequest("GET", "/chainB", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	w := httptest.NewRecorder()
	server.router.ServeHTTP(w, req)

	if w.Code == http.StatusServiceUnavailable {
		t.Errorf("Expected fallback WS endpoint to be selected when primaries are unhealthy, got 503")
	}
}

// TestHTTPSelection_NoFallbackAvailable tests HTTP selection when no fallback is available.
func TestHTTPSelection_NoFallbackAvailable(t *testing.T) {
	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"chainA": {
				"primary1":  config.Endpoint{Provider: "primary1", HTTPURL: "http://primary1", Role: "primary", Type: "full"},
				"fallback1": config.Endpoint{Provider: "fallback1", HTTPURL: "http://fallback1", Role: "fallback", Type: "full"},
			},
		},
	}
	valkeyClient := store.NewMockValkeyClient()
	valkeyClient.PopulateStatuses(map[string]*store.EndpointStatus{
		"chainA:primary1":  {HasHTTP: true, HealthyHTTP: false},
		"chainA:fallback1": {HasHTTP: true, HealthyHTTP: false},
	})
	server := NewServer(cfg, valkeyClient, createTestConfig())
	server.forwardRequestWithBody = stubForwardRequestWithBody
	server.proxyWebSocket = stubProxyWebSocket

	req := httptest.NewRequest("POST", "/chainA", nil)
	w := httptest.NewRecorder()
	server.router.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("Expected 503 when no healthy endpoints (primary or fallback), got %d", w.Code)
	}
}

// TestWSSelection_NoFallbackAvailable tests WS selection when no fallback is available.
func TestWSSelection_NoFallbackAvailable(t *testing.T) {
	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"chainB": {
				"primary1":  config.Endpoint{Provider: "primary1", WSURL: "ws://primary1", Role: "primary", Type: "full"},
				"fallback1": config.Endpoint{Provider: "fallback1", WSURL: "ws://fallback1", Role: "fallback", Type: "full"},
			},
		},
	}
	valkeyClient := store.NewMockValkeyClient()
	valkeyClient.PopulateStatuses(map[string]*store.EndpointStatus{
		"chainB:primary1":  {HasWS: true, HealthyWS: false},
		"chainB:fallback1": {HasWS: true, HealthyWS: false},
	})
	server := NewServer(cfg, valkeyClient, createTestConfig())
	server.forwardRequestWithBody = stubForwardRequestWithBody
	server.proxyWebSocket = stubProxyWebSocket

	req := httptest.NewRequest("GET", "/chainB", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==") // "the sample nonce" in base64, valid length
	w := httptest.NewRecorder()
	server.router.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("Expected 503 when no healthy WS endpoints (primary or fallback), got %d", w.Code)
	}
}

// TestHTTPSelection_PrimaryHealthyNoFallback tests HTTP selection when primary is healthy and fallback is unhealthy.
func TestHTTPSelection_PrimaryHealthyNoFallback(t *testing.T) {
	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"chainA": {
				"primary1":  config.Endpoint{Provider: "primary1", HTTPURL: "http://primary1", Role: "primary", Type: "full"},
				"fallback1": config.Endpoint{Provider: "fallback1", HTTPURL: "http://fallback1", Role: "fallback", Type: "full"},
			},
		},
	}
	valkeyClient := store.NewMockValkeyClient()
	valkeyClient.PopulateStatuses(map[string]*store.EndpointStatus{
		"chainA:primary1":  {HasHTTP: true, HealthyHTTP: true},
		"chainA:fallback1": {HasHTTP: true, HealthyHTTP: false},
	})
	server := NewServer(cfg, valkeyClient, createTestConfig())
	server.forwardRequestWithBody = stubForwardRequestWithBody
	server.proxyWebSocket = stubProxyWebSocket

	req := httptest.NewRequest("POST", "/chainA", nil)
	w := httptest.NewRecorder()
	server.router.ServeHTTP(w, req)

	if w.Code == http.StatusServiceUnavailable {
		t.Errorf("Expected primary endpoint to be selected when healthy, got 503")
	}
}

// TestHTTPRetryLoop tests the retry loop for HTTP requests.
func TestHTTPRetryLoop(t *testing.T) {
	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"chainA": {
				"ep1": config.Endpoint{Provider: "ep1", HTTPURL: "http://fail1", Role: "primary", Type: "full"},
				"ep2": config.Endpoint{Provider: "ep2", HTTPURL: "http://fail2", Role: "primary", Type: "full"},
				"ep3": config.Endpoint{Provider: "ep3", HTTPURL: "http://success", Role: "primary", Type: "full"},
			},
		},
	}
	valkeyClient := store.NewMockValkeyClient()
	valkeyClient.PopulateStatuses(map[string]*store.EndpointStatus{
		"chainA:ep1": {HasHTTP: true, HealthyHTTP: false},
		"chainA:ep2": {HasHTTP: true, HealthyHTTP: false},
		"chainA:ep3": {HasHTTP: true, HealthyHTTP: true},
	})
	server := NewServer(cfg, valkeyClient, createTestConfig())

	// Create a custom forward function that fails for specific URLs
	server.forwardRequestWithBody = func(w http.ResponseWriter, ctx context.Context, method, targetURL string, bodyBytes []byte, headers http.Header) error {
		if targetURL == "http://success" {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("success"))
			return nil
		}
		return fmt.Errorf("endpoint failed: %s", targetURL)
	}
	server.proxyWebSocket = stubProxyWebSocket

	req := httptest.NewRequest("POST", "/chainA", nil)
	w := httptest.NewRecorder()
	server.router.ServeHTTP(w, req)

	// Should succeed after trying ep1 and ep2, then succeeding with ep3
	if w.Code != http.StatusOK {
		t.Errorf("Expected success after retrying endpoints, got %d", w.Code)
	}
	if w.Body.String() != "success" {
		t.Errorf("Expected 'success' response, got '%s'", w.Body.String())
	}
}

// TestHTTPRetryLoop_AllFail checks that the retry loop returns 502 when all HTTP endpoints fail.
func TestHTTPRetryLoop_AllFail(t *testing.T) {
	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"chainA": {
				"ep1": config.Endpoint{Provider: "ep1", HTTPURL: "http://fail1", Role: "primary", Type: "full"},
				"ep2": config.Endpoint{Provider: "ep2", HTTPURL: "http://fail2", Role: "primary", Type: "full"},
			},
		},
	}
	valkeyClient := store.NewMockValkeyClient()
	valkeyClient.PopulateStatuses(map[string]*store.EndpointStatus{
		"chainA:ep1": {HasHTTP: true, HealthyHTTP: false},
		"chainA:ep2": {HasHTTP: true, HealthyHTTP: false},
	})
	server := NewServer(cfg, valkeyClient, createTestConfig())
	server.forwardRequestWithBody = failingForwardRequestWithBody
	server.proxyWebSocket = stubProxyWebSocket

	req := httptest.NewRequest("POST", "/chainA", nil)
	w := httptest.NewRecorder()
	server.router.ServeHTTP(w, req)

	// Should return 502 after trying all endpoints
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("Expected 503 when all endpoints fail, got %d", w.Code)
	}
}

// TestWSRetryLoop tests the retry loop for WebSocket requests.
func TestWSRetryLoop(t *testing.T) {
	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"chainB": {
				"ep1": config.Endpoint{Provider: "ep1", WSURL: "ws://fail1", Role: "primary", Type: "full"},
				"ep2": config.Endpoint{Provider: "ep2", WSURL: "ws://fail2", Role: "primary", Type: "full"},
				"ep3": config.Endpoint{Provider: "ep3", WSURL: "ws://success", Role: "primary", Type: "full"},
			},
		},
	}
	valkeyClient := store.NewMockValkeyClient()
	valkeyClient.PopulateStatuses(map[string]*store.EndpointStatus{
		"chainB:ep1": {HasWS: true, HealthyWS: false},
		"chainB:ep2": {HasWS: true, HealthyWS: false},
		"chainB:ep3": {HasWS: true, HealthyWS: true},
	})
	server := NewServer(cfg, valkeyClient, createTestConfig())
	server.forwardRequestWithBody = stubForwardRequestWithBody

	// Create a custom proxy function that fails for specific URLs
	server.proxyWebSocket = func(w http.ResponseWriter, r *http.Request, backendURL string) error {
		if backendURL == "ws://success" {
			w.WriteHeader(http.StatusSwitchingProtocols)
			return nil
		}
		return fmt.Errorf("websocket endpoint failed: %s", backendURL)
	}

	req := httptest.NewRequest("GET", "/chainB", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	w := httptest.NewRecorder()
	server.router.ServeHTTP(w, req)

	// Should succeed after trying ep1 and ep2, then succeeding with ep3
	if w.Code != http.StatusSwitchingProtocols {
		t.Errorf("Expected success after retrying WebSocket endpoints, got %d", w.Code)
	}
}

// TestWSRetryLoop_AllFail checks that the retry loop returns 502 when all WebSocket endpoints fail.
func TestWSRetryLoop_AllFail(t *testing.T) {
	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"chainB": {
				"ep1": config.Endpoint{Provider: "ep1", WSURL: "ws://fail1", Role: "primary", Type: "full"},
				"ep2": config.Endpoint{Provider: "ep2", WSURL: "ws://fail2", Role: "primary", Type: "full"},
			},
		},
	}
	valkeyClient := store.NewMockValkeyClient()
	valkeyClient.PopulateStatuses(map[string]*store.EndpointStatus{
		"chainB:ep1": {HasWS: true, HealthyWS: false},
		"chainB:ep2": {HasWS: true, HealthyWS: false},
	})
	server := NewServer(cfg, valkeyClient, createTestConfig())
	server.forwardRequestWithBody = stubForwardRequestWithBody
	server.proxyWebSocket = func(w http.ResponseWriter, r *http.Request, backendURL string) error {
		return fmt.Errorf("websocket endpoint failed: %s", backendURL)
	}

	req := httptest.NewRequest("GET", "/chainB", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	w := httptest.NewRecorder()
	server.router.ServeHTTP(w, req)

	// Should return 502 after trying all endpoints
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("Expected 503 when all WebSocket endpoints fail, got %d", w.Code)
	}
}

// TestIsExpectedWSClose tests the isExpectedWSClose helper for WebSocket closure handling.
func TestIsExpectedWSClose(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{"nil error", nil, false},
		{"CloseNormalClosure", &websocket.CloseError{Code: websocket.CloseNormalClosure}, true},
		{"CloseGoingAway", &websocket.CloseError{Code: websocket.CloseGoingAway}, true},
		{"CloseNoStatusReceived", &websocket.CloseError{Code: websocket.CloseNoStatusReceived}, true},
		{"CloseAbnormalClosure", &websocket.CloseError{Code: websocket.CloseAbnormalClosure}, true},
		{"CloseProtocolError", &websocket.CloseError{Code: websocket.CloseProtocolError}, false},
		{"io.EOF", io.EOF, true},
		{"io.ErrUnexpectedEOF", io.ErrUnexpectedEOF, true},
		{"generic error", fmt.Errorf("some other error"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isExpectedWSClose(tt.err); got != tt.expected {
				t.Errorf("isExpectedWSClose(%v) = %v, want %v", tt.err, got, tt.expected)
			}
		})
	}
}

// TestMarkEndpointUnhealthy_HTTP tests marking an endpoint unhealthy for HTTP.
func TestMarkEndpointUnhealthy_HTTP(t *testing.T) {
	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"chainA": {
				"ep1": config.Endpoint{Provider: "ep1", HTTPURL: "http://fail", Role: "primary", Type: "full"},
			},
		},
	}
	valkeyClient := store.NewMockValkeyClient()
	valkeyClient.PopulateStatuses(map[string]*store.EndpointStatus{
		"chainA:ep1": {HasHTTP: true, HealthyHTTP: true},
	})
	server := NewServer(cfg, valkeyClient, createTestConfig())

	// Simulate a failed HTTP request
	err := server.defaultForwardRequestWithBodyFunc(httptest.NewRecorder(), context.Background(), "POST", "http://fail", nil, http.Header{})
	if err == nil {
		t.Error("Expected error from failed HTTP request")
	}

	status, _ := valkeyClient.GetEndpointStatus(context.Background(), "chainA", "ep1")
	if status.HealthyHTTP {
		t.Error("Expected HealthyHTTP to be false after failed request")
	}
}

// TestMarkEndpointUnhealthy_WS tests marking an endpoint unhealthy for WS.
// Note: We cannot fully simulate a WebSocket upgrade with httptest.NewRecorder because it does not implement http.Hijacker.
// Instead, we directly test the marking logic here.
func TestMarkEndpointUnhealthy_WS(t *testing.T) {
	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"chainB": {
				"ep1": config.Endpoint{Provider: "ep1", WSURL: "ws://fail", Role: "primary", Type: "full"},
			},
		},
	}
	valkeyClient := store.NewMockValkeyClient()
	valkeyClient.PopulateStatuses(map[string]*store.EndpointStatus{
		"chainB:ep1": {HasWS: true, HealthyWS: true},
	})
	server := NewServer(cfg, valkeyClient, createTestConfig())

	// Directly call the marking logic
	server.markEndpointUnhealthyProtocol("chainB", "ep1", "ws")

	status, _ := valkeyClient.GetEndpointStatus(context.Background(), "chainB", "ep1")
	if status.HealthyWS {
		t.Error("Expected HealthyWS to be false after marking unhealthy for WS")
	}
}

func TestHandleRateLimit(t *testing.T) {
	// Create a test config
	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"ethereum": {
				"test-endpoint": config.Endpoint{
					Provider: "test-provider",
					Role:     "primary",
					Type:     "full",
					HTTPURL:  "http://test-url.com",
					RateLimitRecovery: &config.RateLimitRecovery{
						BackoffMultiplier: 2.0,
						InitialBackoff:    60,
						MaxBackoff:        300,
						MaxRetries:        5,
						RequiredSuccesses: 2,
						ResetAfter:        3600,
					},
				},
			},
		},
	}

	// Create test app config
	appConfig := &helpers.LoadedConfig{
		ProxyMaxRetries:    3,
		ProxyTimeout:       15,
		ProxyTimeoutPerTry: 5,
	}

	// Create mock Valkey client
	mockValkey := store.NewMockValkeyClient()

	// Create server
	server := NewServer(cfg, mockValkey, appConfig)

	// Test handling rate limit
	server.handleRateLimit("ethereum", "test-endpoint", "http", health.RateLimitSignal{IsRateLimited: true})

	// Verify rate limit state was set
	state, err := mockValkey.GetRateLimitState(context.Background(), "ethereum", "test-endpoint")
	if err != nil {
		t.Fatalf("Failed to get rate limit state: %v", err)
	}

	if !state.RateLimited {
		t.Error("Expected endpoint to be marked as rate limited")
	}

	if state.RecoveryAttempts != 0 {
		t.Errorf("Expected recovery attempts to be 0, got %d", state.RecoveryAttempts)
	}

	if state.ConsecutiveSuccess != 0 {
		t.Errorf("Expected consecutive success to be 0, got %d", state.ConsecutiveSuccess)
	}
}

func TestGetAvailableEndpointsSkipsRateLimited(t *testing.T) {
	// Create a test config
	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"ethereum": {
				"healthy-endpoint": config.Endpoint{
					Provider: "test-provider-1",
					Role:     "primary",
					Type:     "full",
					HTTPURL:  "http://healthy-endpoint.com",
				},
				"rate-limited-endpoint": config.Endpoint{
					Provider: "test-provider-2",
					Role:     "primary",
					Type:     "full",
					HTTPURL:  "http://rate-limited-endpoint.com",
				},
			},
		},
	}

	// Create test app config
	appConfig := &helpers.LoadedConfig{
		ProxyMaxRetries:    3,
		ProxyTimeout:       15,
		ProxyTimeoutPerTry: 5,
	}

	// Create mock Valkey client
	mockValkey := store.NewMockValkeyClient()

	// Set up endpoint statuses
	healthyStatus := store.EndpointStatus{
		HasHTTP:     true,
		HealthyHTTP: true,
	}
	rateLimitedStatus := store.EndpointStatus{
		HasHTTP:     true,
		HealthyHTTP: true, // Still healthy but rate limited
	}

	mockValkey.UpdateEndpointStatus(context.Background(), "ethereum", "healthy-endpoint", healthyStatus)
	mockValkey.UpdateEndpointStatus(context.Background(), "ethereum", "rate-limited-endpoint", rateLimitedStatus)

	// Set rate limit state for one endpoint
	rateLimitState := store.RateLimitState{
		ConsecutiveSuccess: 0,
		CurrentBackoff:     30,
		FirstRateLimited:   time.Now().Add(-5 * time.Minute),
		LastRecoveryCheck:  time.Now(),
		RateLimited:        true,
		RecoveryAttempts:   1,
	}
	mockValkey.SetRateLimitState(context.Background(), "ethereum", "rate-limited-endpoint", rateLimitState)

	// Create server
	server := NewServer(cfg, mockValkey, appConfig)

	// Get available endpoints
	endpoints := server.getAvailableEndpoints("ethereum", false, false)

	// Should only have the healthy endpoint, not the rate-limited one
	if len(endpoints) != 1 {
		t.Errorf("Expected 1 available endpoint, got %d", len(endpoints))
	}

	if len(endpoints) > 0 && endpoints[0].ID != "healthy-endpoint" {
		t.Errorf("Expected healthy-endpoint, got %s", endpoints[0].ID)
	}
}

func TestRateLimitError(t *testing.T) {
	err := &RateLimitError{
		StatusCode: 429,
		Message:    "Too Many Requests",
	}

	if err.Error() != "Too Many Requests" {
		t.Errorf("Expected 'Too Many Requests', got '%s'", err.Error())
	}

	if err.StatusCode != 429 {
		t.Errorf("Expected status code 429, got %d", err.StatusCode)
	}
}

func TestServerGetRateLimitHandler(t *testing.T) {
	// Create a test config
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

	// Create test app config
	appConfig := &helpers.LoadedConfig{
		ProxyMaxRetries:    3,
		ProxyTimeout:       15,
		ProxyTimeoutPerTry: 5,
	}

	// Create mock Valkey client
	mockValkey := store.NewMockValkeyClient()

	// Create server
	server := NewServer(cfg, mockValkey, appConfig)

	// Get rate limit handler
	handler := server.GetRateLimitHandler()

	if handler == nil {
		t.Error("Expected rate limit handler to be returned")
	}

	// Test that handler works
	handler("ethereum", "test-endpoint", "http", health.RateLimitSignal{IsRateLimited: true})

	// Verify rate limit state was set
	state, err := mockValkey.GetRateLimitState(context.Background(), "ethereum", "test-endpoint")
	if err != nil {
		t.Fatalf("Failed to get rate limit state: %v", err)
	}

	if !state.RateLimited {
		t.Error("Expected endpoint to be marked as rate limited")
	}
}

// TestEphemeralChecksEnabled tests that updateEndpointHealthState is gated by the ephemeralChecksEnabled flag.
func TestEphemeralChecksEnabled(t *testing.T) {
	t.Run("Enabled", func(t *testing.T) {
		cfg := &config.Config{
			Endpoints: map[string]config.ChainEndpoints{
				"chainA": {
					"ep1": config.Endpoint{Provider: "ep1", HTTPURL: "http://a", Role: "primary", Type: "full"},
				},
			},
		}
		valkeyClient := store.NewMockValkeyClient()
		valkeyClient.PopulateStatuses(map[string]*store.EndpointStatus{
			"chainA:ep1": {HasHTTP: true, HealthyHTTP: true},
		})
		appConfig := &helpers.LoadedConfig{
			EphemeralChecksEnabled:   true,
			EndpointFailureThreshold: 1,
			EndpointSuccessThreshold: 1,
			ProxyMaxRetries:          3,
			ProxyTimeout:             15,
			ProxyTimeoutPerTry:       5,
		}
		srv := NewServer(cfg, valkeyClient, appConfig)

		// Call updateEndpointHealthState with a failure
		srv.updateEndpointHealthState("chainA", "ep1", "http", false)

		// With threshold=1, the endpoint should be marked unhealthy
		status, _ := valkeyClient.GetEndpointStatus(context.Background(), "chainA", "ep1")
		if status.HealthyHTTP {
			t.Error("Expected HealthyHTTP to be false when ephemeral checks are enabled")
		}
	})

	t.Run("Disabled", func(t *testing.T) {
		cfg := &config.Config{
			Endpoints: map[string]config.ChainEndpoints{
				"chainA": {
					"ep1": config.Endpoint{Provider: "ep1", HTTPURL: "http://a", Role: "primary", Type: "full"},
				},
			},
		}
		valkeyClient := store.NewMockValkeyClient()
		valkeyClient.PopulateStatuses(map[string]*store.EndpointStatus{
			"chainA:ep1": {HasHTTP: true, HealthyHTTP: true},
		})
		appConfig := &helpers.LoadedConfig{
			EphemeralChecksEnabled:   false,
			EndpointFailureThreshold: 1,
			EndpointSuccessThreshold: 1,
			ProxyMaxRetries:          3,
			ProxyTimeout:             15,
			ProxyTimeoutPerTry:       5,
		}
		srv := NewServer(cfg, valkeyClient, appConfig)

		// Call updateEndpointHealthState with a failure
		srv.updateEndpointHealthState("chainA", "ep1", "http", false)

		// The function should be a no-op; endpoint stays healthy
		status, _ := valkeyClient.GetEndpointStatus(context.Background(), "chainA", "ep1")
		if !status.HealthyHTTP {
			t.Error("Expected HealthyHTTP to remain true when ephemeral checks are disabled")
		}
	})
}

// TestInitialBackoffForSignal tests that handleRateLimit's backoff seeding prefers a
// parsed Retry-After, falls back to the endpoint's own (or default) MaxBackoff for a
// daily-quota signal, and leaves 0 (scheduler default) for a plain signal.
func TestInitialBackoffForSignal(t *testing.T) {
	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"chainA": {
				"with-override": config.Endpoint{
					Provider:          "infura",
					Role:              "primary",
					Type:              "full",
					HTTPURL:           "http://with-override",
					RateLimitRecovery: &config.RateLimitRecovery{MaxBackoff: 999},
				},
				"no-override": config.Endpoint{
					Provider: "infura",
					Role:     "primary",
					Type:     "full",
					HTTPURL:  "http://no-override",
				},
			},
		},
	}
	valkeyClient := store.NewMockValkeyClient()
	server := NewServer(cfg, valkeyClient, createTestConfig())

	tests := []struct {
		name       string
		endpointID string
		signal     health.RateLimitSignal
		expected   int
	}{
		{"retry-after takes priority over daily quota", "no-override", health.RateLimitSignal{RetryAfter: 42 * time.Second, IsDailyQuota: true}, 42},
		{"daily quota uses endpoint's own MaxBackoff override", "with-override", health.RateLimitSignal{IsDailyQuota: true}, 999},
		{"daily quota without override uses default MaxBackoff", "no-override", health.RateLimitSignal{IsDailyQuota: true}, config.DefaultRateLimitRecovery().MaxBackoff},
		{"plain rate limit signal leaves 0 for scheduler to guess InitialBackoff", "no-override", health.RateLimitSignal{IsRateLimited: true}, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := server.initialBackoffForSignal("chainA", tt.endpointID, tt.signal); got != tt.expected {
				t.Errorf("initialBackoffForSignal() = %d, want %d", got, tt.expected)
			}
		})
	}
}

// TestBodyCarriesRateLimitSignal tests the pure JSON-RPC body scanner used to close the
// blind spot where a provider (e.g. Alchemy on batch requests) signals rate limiting
// only inside a 200 response body, not via HTTP status.
func TestBodyCarriesRateLimitSignal(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		expected bool
	}{
		{"single object with rate limit error", `{"jsonrpc":"2.0","id":1,"error":{"code":-32005,"message":"Request limit exceeded"}}`, true},
		{"single object success", `{"jsonrpc":"2.0","id":1,"result":"0x1"}`, false},
		{"batch with one rate limited element", `[{"jsonrpc":"2.0","id":1,"result":"0x1"},{"jsonrpc":"2.0","id":2,"error":{"code":-32005,"message":"limit"}}]`, true},
		{"batch all success", `[{"jsonrpc":"2.0","id":1,"result":"0x1"},{"jsonrpc":"2.0","id":2,"result":"0x2"}]`, false},
		{"batch with unrelated error", `[{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"method not found"}}]`, false},
		{"non-JSON body", `not json at all`, false},
		{"unrelated single JSON-RPC error", `{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"method not found"}}`, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sig := bodyCarriesRateLimitSignal("alchemy", []byte(tt.body), nil)
			if sig.IsRateLimited != tt.expected {
				t.Errorf("bodyCarriesRateLimitSignal() = %v, want %v", sig.IsRateLimited, tt.expected)
			}
		})
	}
}

// TestDefaultForwardRequestWithBodyFunc429ParsesRetryAfter tests that a 429 response
// carrying a Retry-After header seeds RateLimitState.CurrentBackoff precisely instead of
// leaving it to the scheduler's guessed InitialBackoff.
func TestDefaultForwardRequestWithBodyFunc429ParsesRetryAfter(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer ts.Close()

	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"chainA": {
				"ep1": config.Endpoint{Provider: "alchemy", HTTPURL: ts.URL, Role: "primary", Type: "full"},
			},
		},
	}
	valkeyClient := store.NewMockValkeyClient()
	server := NewServer(cfg, valkeyClient, createTestConfig())

	err := server.defaultForwardRequestWithBodyFunc(httptest.NewRecorder(), context.Background(), "POST", ts.URL, nil, http.Header{})
	if err == nil {
		t.Fatal("Expected error from 429 response")
	}

	state, stateErr := valkeyClient.GetRateLimitState(context.Background(), "chainA", "ep1")
	if stateErr != nil {
		t.Fatalf("Failed to get rate limit state: %v", stateErr)
	}
	if !state.RateLimited {
		t.Error("Expected endpoint to be marked as rate limited")
	}
	if state.CurrentBackoff != 5 {
		t.Errorf("Expected CurrentBackoff to be seeded to 5 from Retry-After, got %d", state.CurrentBackoff)
	}
}

// TestDefaultForwardRequestWithBodyFunc402InfuraSeedsFromMaxBackoff tests that Infura's
// HTTP 402 daily-credit-cap signal seeds CurrentBackoff from the endpoint's configured
// MaxBackoff rather than the scheduler's normal short InitialBackoff.
func TestDefaultForwardRequestWithBodyFunc402InfuraSeedsFromMaxBackoff(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
	}))
	defer ts.Close()

	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"chainA": {
				"ep1": config.Endpoint{
					Provider:          "infura",
					HTTPURL:           ts.URL,
					Role:              "primary",
					Type:              "full",
					RateLimitRecovery: &config.RateLimitRecovery{MaxBackoff: 12345},
				},
			},
		},
	}
	valkeyClient := store.NewMockValkeyClient()
	server := NewServer(cfg, valkeyClient, createTestConfig())

	err := server.defaultForwardRequestWithBodyFunc(httptest.NewRecorder(), context.Background(), "POST", ts.URL, nil, http.Header{})
	if err == nil {
		t.Fatal("Expected error from 402 response")
	}

	state, stateErr := valkeyClient.GetRateLimitState(context.Background(), "chainA", "ep1")
	if stateErr != nil {
		t.Fatalf("Failed to get rate limit state: %v", stateErr)
	}
	if !state.RateLimited {
		t.Error("Expected endpoint to be marked as rate limited")
	}
	if state.CurrentBackoff != 12345 {
		t.Errorf("Expected CurrentBackoff to be seeded from the endpoint's MaxBackoff override (12345), got %d", state.CurrentBackoff)
	}
}

// TestDefaultForwardRequestWithBodyFunc402NonInfuraNotRateLimited tests that a 402 from a
// non-Infura provider is NOT treated as a rate-limit signal, since 402 is only a documented
// Infura convention.
func TestDefaultForwardRequestWithBodyFunc402NonInfuraNotRateLimited(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
	}))
	defer ts.Close()

	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"chainA": {
				"ep1": config.Endpoint{Provider: "alchemy", HTTPURL: ts.URL, Role: "primary", Type: "full"},
			},
		},
	}
	valkeyClient := store.NewMockValkeyClient()
	server := NewServer(cfg, valkeyClient, createTestConfig())

	err := server.defaultForwardRequestWithBodyFunc(httptest.NewRecorder(), context.Background(), "POST", ts.URL, nil, http.Header{})
	if err == nil {
		t.Fatal("Expected error from 402 response")
	}

	state, stateErr := valkeyClient.GetRateLimitState(context.Background(), "chainA", "ep1")
	if stateErr != nil {
		t.Fatalf("Failed to get rate limit state: %v", stateErr)
	}
	if state.RateLimited {
		t.Error("Expected a non-Infura 402 to NOT be treated as a rate-limit signal")
	}
}

// TestDefaultForwardRequestWithBodyFuncDetectsEmbeddedBatchRateLimit tests that a 200
// response carrying a rate-limit error embedded in one element of a JSON-RPC batch array
// is treated as a failed attempt (retried against a different endpoint), rather than
// forwarded to the client with the embedded error silently mixed in.
func TestDefaultForwardRequestWithBodyFuncDetectsEmbeddedBatchRateLimit(t *testing.T) {
	batchBody := `[{"jsonrpc":"2.0","id":1,"result":"0x1"},{"jsonrpc":"2.0","id":2,"error":{"code":-32005,"message":"Request limit exceeded"}}]`
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(batchBody))
	}))
	defer ts.Close()

	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"chainA": {
				"ep1": config.Endpoint{Provider: "alchemy", HTTPURL: ts.URL, Role: "primary", Type: "full"},
			},
		},
	}
	valkeyClient := store.NewMockValkeyClient()
	// Pre-populate as healthy so we can confirm the 2xx transaction is NOT marked
	// unhealthy (only flagged as rate limited) - it genuinely succeeded at the HTTP level.
	valkeyClient.PopulateStatuses(map[string]*store.EndpointStatus{
		"chainA:ep1": {HasHTTP: true, HealthyHTTP: true},
	})
	appConfig := createTestConfig()
	appConfig.EndpointFailureThreshold = 1
	server := NewServer(cfg, valkeyClient, appConfig)

	rec := httptest.NewRecorder()
	err := server.defaultForwardRequestWithBodyFunc(rec, context.Background(), "POST", ts.URL, nil, http.Header{})
	if err == nil {
		t.Fatal("Expected an error so the caller retries with a different endpoint")
	}
	if rec.Body.Len() != 0 {
		t.Errorf("Expected nothing written to the client, got %q", rec.Body.String())
	}

	state, stateErr := valkeyClient.GetRateLimitState(context.Background(), "chainA", "ep1")
	if stateErr != nil {
		t.Fatalf("Failed to get rate limit state: %v", stateErr)
	}
	if !state.RateLimited {
		t.Error("Expected endpoint to be marked rate limited from the embedded batch error")
	}

	status, statusErr := valkeyClient.GetEndpointStatus(context.Background(), "chainA", "ep1")
	if statusErr != nil {
		t.Fatalf("Failed to get endpoint status: %v", statusErr)
	}
	if !status.HealthyHTTP {
		t.Error("Expected HealthyHTTP to remain true - a 2xx response should not be marked unhealthy, only rate limited")
	}
}

// TestDefaultForwardRequestWithBodyFuncNoDuplicateHeadersAcrossRetries guards against a
// regression where response headers were copied onto w unconditionally, before the
// embedded-rate-limit body scan decided whether to abort. Since handleRequestHTTP's
// retry loop reuses the same http.ResponseWriter across attempts, an aborted attempt's
// headers were left sitting in w's header map and then added to (not replaced by) the
// next successful attempt's headers - producing duplicate header values in the response
// actually sent to the client.
func TestDefaultForwardRequestWithBodyFuncNoDuplicateHeadersAcrossRetries(t *testing.T) {
	rateLimitedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Test-Header", "from-rate-limited")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`[{"jsonrpc":"2.0","id":1,"result":"0x1"},{"jsonrpc":"2.0","id":2,"error":{"code":-32005,"message":"Request limit exceeded"}}]`))
	}))
	defer rateLimitedServer.Close()

	healthyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Test-Header", "from-healthy")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x2"}`))
	}))
	defer healthyServer.Close()

	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"ethereum": {
				"flaky":    config.Endpoint{Provider: "alchemy", HTTPURL: rateLimitedServer.URL, Role: "primary", Type: "full"},
				"reliable": config.Endpoint{Provider: "drpc", HTTPURL: healthyServer.URL, Role: "primary", Type: "full"},
			},
		},
	}
	valkeyClient := store.NewMockValkeyClient()
	server := NewServer(cfg, valkeyClient, createTestConfig())

	// Simulate exactly what handleRequestHTTP's retry loop does: the same
	// http.ResponseWriter passed to a first attempt that aborts (embedded rate-limit
	// signal, no write), then to a second attempt that succeeds and writes for real.
	w := httptest.NewRecorder()

	if err := server.defaultForwardRequestWithBodyFunc(w, context.Background(), "POST", rateLimitedServer.URL, nil, http.Header{}); err == nil {
		t.Fatal("Expected the first attempt (embedded rate-limit signal) to return an error")
	}

	if err := server.defaultForwardRequestWithBodyFunc(w, context.Background(), "POST", healthyServer.URL, nil, http.Header{}); err != nil {
		t.Fatalf("Expected the second attempt to succeed, got error: %v", err)
	}

	values := w.Header().Values("X-Test-Header")
	if len(values) != 1 {
		t.Fatalf("Expected exactly one X-Test-Header value in the final response, got %v (duplicate headers leaked across retries)", values)
	}
	if values[0] != "from-healthy" {
		t.Errorf("Expected the surviving header to be from the endpoint that actually succeeded, got %q", values[0])
	}
}

// TestDefaultProxyWebSocketHandshake429ParsesRetryAfter tests that a 429 during the WS
// handshake dial (before any client upgrade) carries a Retry-After header through to the
// returned RateLimitError's Signal field.
func TestDefaultProxyWebSocketHandshake429ParsesRetryAfter(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer ts.Close()

	backendURL := "ws" + strings.TrimPrefix(ts.URL, "http")

	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"chainA": {
				"ep1": config.Endpoint{Provider: "alchemy", WSURL: backendURL, Role: "primary", Type: "full"},
			},
		},
	}
	valkeyClient := store.NewMockValkeyClient()
	server := NewServer(cfg, valkeyClient, createTestConfig())

	err := server.defaultProxyWebSocket(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil), backendURL)
	if err == nil {
		t.Fatal("Expected an error from the failed WS handshake")
	}
	rlErr, ok := err.(*RateLimitError)
	if !ok {
		t.Fatalf("Expected *RateLimitError, got %T: %v", err, err)
	}
	if rlErr.Signal.RetryAfter != 7*time.Second {
		t.Errorf("Expected RetryAfter to be 7s, got %v", rlErr.Signal.RetryAfter)
	}
}
