package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"aetherlay/internal/config"
	"aetherlay/internal/store"

	"github.com/gorilla/websocket"
)

// stubForwardRequest is a stub for HTTP forwarding in tests.
func stubForwardRequest(w http.ResponseWriter, r *http.Request, targetURL string) error {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("stubbed"))
	return nil
}

// stubProxyWebSocket is a stub for WebSocket proxying in tests.
func stubProxyWebSocket(w http.ResponseWriter, r *http.Request, backendURL string) error {
	w.WriteHeader(http.StatusSwitchingProtocols)
	return nil
}

// failingForwardRequest simulates a failing endpoint for testing retry logic.
func failingForwardRequest(w http.ResponseWriter, r *http.Request, targetURL string) error {
	return fmt.Errorf("endpoint failed: %s", targetURL)
}

// TestServerHealthCheck tests the /health endpoint handler.
func TestServerHealthCheck(t *testing.T) {
	cfg := &config.Config{}
	redisClient := store.NewMockRedisClient()
	server := NewServer(cfg, redisClient)

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
	redisClient := store.NewMockRedisClient()
	redisClient.PopulateStatuses(map[string]*store.EndpointStatus{
		"chainA:ep1": {HasHTTP: true, HealthyHTTP: true},
		"chainA:ep2": {HasHTTP: true, HealthyHTTP: false},
	})
	server := NewServer(cfg, redisClient)
	server.forwardRequest = stubForwardRequest
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
	redisClient := store.NewMockRedisClient()
	redisClient.PopulateStatuses(map[string]*store.EndpointStatus{
		"chainA:ep1": {HasHTTP: true, HealthyHTTP: false},
	})
	server := NewServer(cfg, redisClient)
	server.forwardRequest = stubForwardRequest
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
	redisClient := store.NewMockRedisClient()
	redisClient.PopulateStatuses(map[string]*store.EndpointStatus{
		"chainB:ep1": {HasWS: true, HealthyWS: true},
		"chainB:ep2": {HasWS: true, HealthyWS: false},
	})
	server := NewServer(cfg, redisClient)
	server.forwardRequest = stubForwardRequest
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
	redisClient := store.NewMockRedisClient()
	redisClient.PopulateStatuses(map[string]*store.EndpointStatus{
		"chainB:ep1": {HasWS: true, HealthyWS: false},
	})
	server := NewServer(cfg, redisClient)
	server.forwardRequest = stubForwardRequest
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
	redisClient := store.NewMockRedisClient()
	redisClient.PopulateStatuses(map[string]*store.EndpointStatus{
		"chainA:primary1":  {HasHTTP: true, HealthyHTTP: false},
		"chainA:primary2":  {HasHTTP: true, HealthyHTTP: false},
		"chainA:fallback1": {HasHTTP: true, HealthyHTTP: true},
		"chainA:fallback2": {HasHTTP: true, HealthyHTTP: true},
	})
	server := NewServer(cfg, redisClient)
	server.forwardRequest = stubForwardRequest
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
	redisClient := store.NewMockRedisClient()
	redisClient.PopulateStatuses(map[string]*store.EndpointStatus{
		"chainB:primary1":  {HasWS: true, HealthyWS: false},
		"chainB:primary2":  {HasWS: true, HealthyWS: false},
		"chainB:fallback1": {HasWS: true, HealthyWS: true},
		"chainB:fallback2": {HasWS: true, HealthyWS: true},
	})
	server := NewServer(cfg, redisClient)
	server.forwardRequest = stubForwardRequest
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
	redisClient := store.NewMockRedisClient()
	redisClient.PopulateStatuses(map[string]*store.EndpointStatus{
		"chainA:primary1":  {HasHTTP: true, HealthyHTTP: false},
		"chainA:fallback1": {HasHTTP: true, HealthyHTTP: false},
	})
	server := NewServer(cfg, redisClient)
	server.forwardRequest = stubForwardRequest
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
	redisClient := store.NewMockRedisClient()
	redisClient.PopulateStatuses(map[string]*store.EndpointStatus{
		"chainB:primary1":  {HasWS: true, HealthyWS: false},
		"chainB:fallback1": {HasWS: true, HealthyWS: false},
	})
	server := NewServer(cfg, redisClient)
	server.forwardRequest = stubForwardRequest
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
	redisClient := store.NewMockRedisClient()
	redisClient.PopulateStatuses(map[string]*store.EndpointStatus{
		"chainA:primary1":  {HasHTTP: true, HealthyHTTP: true},
		"chainA:fallback1": {HasHTTP: true, HealthyHTTP: false},
	})
	server := NewServer(cfg, redisClient)
	server.forwardRequest = stubForwardRequest
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
	redisClient := store.NewMockRedisClient()
	redisClient.PopulateStatuses(map[string]*store.EndpointStatus{
		"chainA:ep1": {HasHTTP: true, HealthyHTTP: false},
		"chainA:ep2": {HasHTTP: true, HealthyHTTP: false},
		"chainA:ep3": {HasHTTP: true, HealthyHTTP: true},
	})
	server := NewServer(cfg, redisClient)

	// Create a custom forward function that fails for specific URLs
	server.forwardRequest = func(w http.ResponseWriter, r *http.Request, targetURL string) error {
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
	redisClient := store.NewMockRedisClient()
	redisClient.PopulateStatuses(map[string]*store.EndpointStatus{
		"chainA:ep1": {HasHTTP: true, HealthyHTTP: false},
		"chainA:ep2": {HasHTTP: true, HealthyHTTP: false},
	})
	server := NewServer(cfg, redisClient)
	server.forwardRequest = failingForwardRequest
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
	redisClient := store.NewMockRedisClient()
	redisClient.PopulateStatuses(map[string]*store.EndpointStatus{
		"chainB:ep1": {HasWS: true, HealthyWS: false},
		"chainB:ep2": {HasWS: true, HealthyWS: false},
		"chainB:ep3": {HasWS: true, HealthyWS: true},
	})
	server := NewServer(cfg, redisClient)
	server.forwardRequest = stubForwardRequest

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
	redisClient := store.NewMockRedisClient()
	redisClient.PopulateStatuses(map[string]*store.EndpointStatus{
		"chainB:ep1": {HasWS: true, HealthyWS: false},
		"chainB:ep2": {HasWS: true, HealthyWS: false},
	})
	server := NewServer(cfg, redisClient)
	server.forwardRequest = stubForwardRequest
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

// TestWebSocketNormalClosureHandling tests normal WebSocket closure handling.
func TestWebSocketNormalClosureHandling(t *testing.T) {
	// Test that normal WebSocket closures don't result in errors
	normalClosureErr := &websocket.CloseError{Code: websocket.CloseNormalClosure}
	goingAwayErr := &websocket.CloseError{Code: websocket.CloseGoingAway}
	protocolErr := &websocket.CloseError{Code: websocket.CloseProtocolError}

	// These should be treated as normal closures (no error)
	if !isNormalWebSocketClosure(normalClosureErr) {
		t.Error("CloseNormalClosure should be treated as normal closure")
	}
	if !isNormalWebSocketClosure(goingAwayErr) {
		t.Error("CloseGoingAway should be treated as normal closure")
	}

	// This should be treated as an error
	if isNormalWebSocketClosure(protocolErr) {
		t.Error("CloseProtocolError should not be treated as normal closure")
	}

	// Non-WebSocket errors should not be treated as normal closures
	otherErr := fmt.Errorf("some other error")
	if isNormalWebSocketClosure(otherErr) {
		t.Error("Non-WebSocket errors should not be treated as normal closure")
	}
}

// isNormalWebSocketClosure is a helper to test normal closure logic.
func isNormalWebSocketClosure(err error) bool {
	if err == nil {
		return false
	}
	if closeErr, ok := err.(*websocket.CloseError); ok {
		return closeErr.Code == websocket.CloseNormalClosure || closeErr.Code == websocket.CloseGoingAway
	}
	return false
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
	redisClient := store.NewMockRedisClient()
	redisClient.PopulateStatuses(map[string]*store.EndpointStatus{
		"chainA:ep1": {HasHTTP: true, HealthyHTTP: true},
	})
	server := NewServer(cfg, redisClient)

	// Simulate a failed HTTP request
	err := server.defaultForwardRequest(httptest.NewRecorder(), httptest.NewRequest("POST", "/chainA", nil), "http://fail")
	if err == nil {
		t.Error("Expected error from failed HTTP request")
	}

	status, _ := redisClient.GetEndpointStatus(context.Background(), "chainA", "ep1")
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
	redisClient := store.NewMockRedisClient()
	redisClient.PopulateStatuses(map[string]*store.EndpointStatus{
		"chainB:ep1": {HasWS: true, HealthyWS: true},
	})
	server := NewServer(cfg, redisClient)

	// Directly call the marking logic
	server.markEndpointUnhealthyProtocol("chainB", "ep1", "ws")

	status, _ := redisClient.GetEndpointStatus(context.Background(), "chainB", "ep1")
	if status.HealthyWS {
		t.Error("Expected HealthyWS to be false after marking unhealthy for WS")
	}
}
