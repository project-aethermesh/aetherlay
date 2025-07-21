package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"aetherlay/internal/config"
	"aetherlay/internal/store"

	"github.com/gorilla/websocket"
)

// mockRedisClient allows setting EndpointStatus for endpoints
// and implements the minimal interface needed for server tests
// (only GetEndpointStatus is used for selection logic)
type mockRedisClient struct {
	statuses map[string]*store.EndpointStatus
	mu       sync.RWMutex
}

func (m *mockRedisClient) GetEndpointStatus(_ context.Context, chain, provider string) (*store.EndpointStatus, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	key := chain + ":" + provider
	status, ok := m.statuses[key]
	if !ok {
		return &store.EndpointStatus{}, nil
	}
	return status, nil
}

func (m *mockRedisClient) GetCombinedRequestCounts(_ context.Context, chain, provider string) (int64, int64, int64, error) {
	return 0, 0, 0, nil
}

func (m *mockRedisClient) IncrementRequestCount(_ context.Context, chain, provider string, requestType string) error {
	return nil
}

func (m *mockRedisClient) UpdateEndpointStatus(_ context.Context, chain, provider string, status store.EndpointStatus) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := chain + ":" + provider
	m.statuses[key] = &status
	return nil
}

// The rest of the RedisClient interface is not needed for these tests

func stubForwardRequest(w http.ResponseWriter, r *http.Request, targetURL string) error {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("stubbed"))
	return nil
}

func stubProxyWebSocket(w http.ResponseWriter, r *http.Request, backendURL string) error {
	w.WriteHeader(http.StatusSwitchingProtocols)
	return nil
}

func TestServerHealthCheck(t *testing.T) {
	cfg := &config.Config{}
	redisClient := &mockRedisClient{statuses: make(map[string]*store.EndpointStatus)}
	server := NewServer(cfg, redisClient)

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()

	server.router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status code %d, got %d", http.StatusOK, w.Code)
	}
}

func TestHTTPSelection_HealthyOnly(t *testing.T) {
	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"chainA": {
				"ep1": config.Endpoint{Provider: "ep1", RPCURL: "http://a", WSURL: "ws://a", Role: "primary", Type: "full"},
				"ep2": config.Endpoint{Provider: "ep2", RPCURL: "http://b", WSURL: "ws://b", Role: "primary", Type: "full"},
			},
		},
	}
	redisClient := &mockRedisClient{statuses: map[string]*store.EndpointStatus{
		"chainA:ep1": {HasHTTP: true, HealthyHTTP: true},
		"chainA:ep2": {HasHTTP: true, HealthyHTTP: false},
	}}
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

func TestHTTPSelection_NoneHealthy(t *testing.T) {
	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"chainA": {
				"ep1": config.Endpoint{Provider: "ep1", RPCURL: "http://a", Role: "primary", Type: "full"},
			},
		},
	}
	redisClient := &mockRedisClient{statuses: map[string]*store.EndpointStatus{
		"chainA:ep1": {HasHTTP: true, HealthyHTTP: false},
	}}
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

func TestWSSelection_HealthyOnly(t *testing.T) {
	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"chainB": {
				"ep1": config.Endpoint{Provider: "ep1", WSURL: "ws://a", Role: "primary", Type: "full"},
				"ep2": config.Endpoint{Provider: "ep2", WSURL: "ws://b", Role: "primary", Type: "full"},
			},
		},
	}
	redisClient := &mockRedisClient{statuses: map[string]*store.EndpointStatus{
		"chainB:ep1": {HasWS: true, HealthyWS: true},
		"chainB:ep2": {HasWS: true, HealthyWS: false},
	}}
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

func TestWSSelection_NoneHealthy(t *testing.T) {
	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"chainB": {
				"ep1": config.Endpoint{Provider: "ep1", WSURL: "ws://a", Role: "primary", Type: "full"},
			},
		},
	}
	redisClient := &mockRedisClient{statuses: map[string]*store.EndpointStatus{
		"chainB:ep1": {HasWS: true, HealthyWS: false},
	}}
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

func TestHTTPSelection_FallbackWhenPrimaryUnhealthy(t *testing.T) {
	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"chainA": {
				"primary1": config.Endpoint{Provider: "primary1", RPCURL: "http://primary1", Role: "primary", Type: "full"},
				"primary2": config.Endpoint{Provider: "primary2", RPCURL: "http://primary2", Role: "primary", Type: "full"},
				"fallback1": config.Endpoint{Provider: "fallback1", RPCURL: "http://fallback1", Role: "fallback", Type: "full"},
				"fallback2": config.Endpoint{Provider: "fallback2", RPCURL: "http://fallback2", Role: "fallback", Type: "full"},
			},
		},
	}
	redisClient := &mockRedisClient{statuses: map[string]*store.EndpointStatus{
		"chainA:primary1": {HasHTTP: true, HealthyHTTP: false}, // Primary unhealthy
		"chainA:primary2": {HasHTTP: true, HealthyHTTP: false}, // Primary unhealthy
		"chainA:fallback1": {HasHTTP: true, HealthyHTTP: true}, // Fallback healthy
		"chainA:fallback2": {HasHTTP: true, HealthyHTTP: false}, // Fallback unhealthy
	}}
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

func TestWSSelection_FallbackWhenPrimaryUnhealthy(t *testing.T) {
	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"chainB": {
				"primary1": config.Endpoint{Provider: "primary1", WSURL: "ws://primary1", Role: "primary", Type: "full"},
				"primary2": config.Endpoint{Provider: "primary2", WSURL: "ws://primary2", Role: "primary", Type: "full"},
				"fallback1": config.Endpoint{Provider: "fallback1", WSURL: "ws://fallback1", Role: "fallback", Type: "full"},
				"fallback2": config.Endpoint{Provider: "fallback2", WSURL: "ws://fallback2", Role: "fallback", Type: "full"},
			},
		},
	}
	redisClient := &mockRedisClient{statuses: map[string]*store.EndpointStatus{
		"chainB:primary1": {HasWS: true, HealthyWS: false}, // Primary unhealthy
		"chainB:primary2": {HasWS: true, HealthyWS: false}, // Primary unhealthy
		"chainB:fallback1": {HasWS: true, HealthyWS: true}, // Fallback healthy
		"chainB:fallback2": {HasWS: true, HealthyWS: false}, // Fallback unhealthy
	}}
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

func TestHTTPSelection_NoFallbackAvailable(t *testing.T) {
	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"chainA": {
				"primary1": config.Endpoint{Provider: "primary1", RPCURL: "http://primary1", Role: "primary", Type: "full"},
				"fallback1": config.Endpoint{Provider: "fallback1", RPCURL: "http://fallback1", Role: "fallback", Type: "full"},
			},
		},
	}
	redisClient := &mockRedisClient{statuses: map[string]*store.EndpointStatus{
		"chainA:primary1": {HasHTTP: true, HealthyHTTP: false}, // Primary unhealthy
		"chainA:fallback1": {HasHTTP: true, HealthyHTTP: false}, // Fallback also unhealthy
	}}
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

func TestWSSelection_NoFallbackAvailable(t *testing.T) {
	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"chainB": {
				"primary1": config.Endpoint{Provider: "primary1", WSURL: "ws://primary1", Role: "primary", Type: "full"},
				"fallback1": config.Endpoint{Provider: "fallback1", WSURL: "ws://fallback1", Role: "fallback", Type: "full"},
			},
		},
	}
	redisClient := &mockRedisClient{statuses: map[string]*store.EndpointStatus{
		"chainB:primary1": {HasWS: true, HealthyWS: false}, // Primary unhealthy
		"chainB:fallback1": {HasWS: true, HealthyWS: false}, // Fallback also unhealthy
	}}
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

func TestHTTPSelection_PrimaryHealthyNoFallback(t *testing.T) {
	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"chainA": {
				"primary1": config.Endpoint{Provider: "primary1", RPCURL: "http://primary1", Role: "primary", Type: "full"},
				"fallback1": config.Endpoint{Provider: "fallback1", RPCURL: "http://fallback1", Role: "fallback", Type: "full"},
			},
		},
	}
	redisClient := &mockRedisClient{statuses: map[string]*store.EndpointStatus{
		"chainA:primary1": {HasHTTP: true, HealthyHTTP: true}, // Primary healthy
		"chainA:fallback1": {HasHTTP: true, HealthyHTTP: false}, // Fallback unhealthy (should not be used)
	}}
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

// Helper function to test the normal closure logic
func isNormalWebSocketClosure(err error) bool {
	if err == nil {
		return false
	}
	if closeErr, ok := err.(*websocket.CloseError); ok {
		return closeErr.Code == websocket.CloseNormalClosure || closeErr.Code == websocket.CloseGoingAway
	}
	return false
}

func TestMarkEndpointUnhealthy_HTTP(t *testing.T) {
	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"chainA": {
				"ep1": config.Endpoint{Provider: "ep1", RPCURL: "http://fail", Role: "primary", Type: "full"},
			},
		},
	}
	redisClient := &mockRedisClient{statuses: map[string]*store.EndpointStatus{
		"chainA:ep1": {HasHTTP: true, HealthyHTTP: true},
	}}
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

// TestMarkEndpointUnhealthy_WS verifies that the server marks an endpoint as unhealthy for WS.
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
	redisClient := &mockRedisClient{statuses: map[string]*store.EndpointStatus{
		"chainB:ep1": {HasWS: true, HealthyWS: true},
	}}
	server := NewServer(cfg, redisClient)

	// Directly call the marking logic
	server.markEndpointUnhealthyProtocol("chainB", "ep1", "ws")

	status, _ := redisClient.GetEndpointStatus(context.Background(), "chainB", "ep1")
	if status.HealthyWS {
		t.Error("Expected HealthyWS to be false after marking unhealthy for WS")
	}
}
