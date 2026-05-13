package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aetherlay/internal/config"
	"aetherlay/internal/helpers"
	"aetherlay/internal/store"

	"github.com/gorilla/websocket"
)

var testWSUpgrader = websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

// newBackendWSServer creates a test WebSocket backend server. The handler
// receives the upgraded connection and can close it however the test requires.
func newBackendWSServer(handler func(*websocket.Conn)) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := testWSUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		handler(conn)
	}))
}

// newWSProxyServer creates an aetherlay server wired to a single WS endpoint
// (backendWSURL) on chain "testchain" / endpoint "ep1", pre-seeded as healthy.
// Thresholds are set to 1 so that a single failure/success immediately updates
// the Valkey status, making assertions straightforward.
func newWSProxyServer(t *testing.T, backendWSURL string) (*httptest.Server, *store.MockValkeyClient) {
	t.Helper()
	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"testchain": {
				"ep1": config.Endpoint{WSURL: backendWSURL, Role: "primary", Type: "full"},
			},
		},
	}
	vk := store.NewMockValkeyClient()
	vk.PopulateStatuses(map[string]*store.EndpointStatus{
		"testchain:ep1": {HasWS: true, HealthyWS: true},
	})
	appCfg := &helpers.LoadedConfig{
		EphemeralChecksEnabled:   true,
		EndpointFailureThreshold: 1,
		EndpointSuccessThreshold: 1,
		ProxyMaxRetries:          1,
		ProxyTimeout:             5,
		ProxyTimeoutPerTry:       5,
	}
	srv := NewServer(cfg, vk, appCfg)
	ts := httptest.NewServer(srv.router)
	t.Cleanup(ts.Close)
	return ts, vk
}

// dialTestWS connects a gorilla WebSocket client to ts at the given path.
func dialTestWS(t *testing.T, ts *httptest.Server, path string) *websocket.Conn {
	t.Helper()
	u := "ws" + strings.TrimPrefix(ts.URL, "http") + path
	conn, _, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", u, err)
	}
	return conn
}

// wsCloseCode reads from conn until it receives a close error and returns the
// WebSocket close code. Returns -1 if the connection closed without a WS close frame.
func wsCloseCode(conn *websocket.Conn) int {
	for {
		_, _, err := conn.ReadMessage()
		if err == nil {
			continue
		}
		if ce, ok := err.(*websocket.CloseError); ok {
			return ce.Code
		}
		return -1
	}
}

// wsIsUnhealthy reports whether ep1 on testchain is currently WS-unhealthy in vk.
func wsIsUnhealthy(t *testing.T, vk *store.MockValkeyClient) bool {
	t.Helper()
	status, err := vk.GetEndpointStatus(context.Background(), "testchain", "ep1")
	if err != nil {
		t.Fatalf("GetEndpointStatus: %v", err)
	}
	return !status.HealthyWS
}

// settle gives aetherlay goroutines a moment to finish processing after the
// client observes the close.
func settle() { time.Sleep(80 * time.Millisecond) }

// --- Backend-initiated closes ---

// TestWSClose_Backend_NormalClosure verifies that a clean 1000 close from the
// backend (e.g., connection time-limit) is NOT counted as a failure.
// The client must receive 1000 and the endpoint must remain healthy.
func TestWSClose_Backend_NormalClosure(t *testing.T) {
	done := make(chan struct{})
	backend := newBackendWSServer(func(conn *websocket.Conn) {
		defer close(done)
		conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, "time limit reached"))
		time.Sleep(20 * time.Millisecond)
		conn.Close()
	})
	defer backend.Close()

	ts, vk := newWSProxyServer(t, "ws"+strings.TrimPrefix(backend.URL, "http"))
	client := dialTestWS(t, ts, "/testchain")
	defer client.Close()

	code := wsCloseCode(client)
	if code != websocket.CloseNormalClosure {
		t.Errorf("client expected close 1000, got %d", code)
	}

	<-done
	settle()

	if wsIsUnhealthy(t, vk) {
		t.Error("endpoint must remain healthy after backend 1000 (Normal Closure)")
	}
}

// TestWSClose_Backend_GoingAway verifies that a 1001 close from the backend
// (planned shutdown) is NOT counted as a failure.
// The client must receive 1001 and the endpoint must remain healthy.
func TestWSClose_Backend_GoingAway(t *testing.T) {
	done := make(chan struct{})
	backend := newBackendWSServer(func(conn *websocket.Conn) {
		defer close(done)
		conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseGoingAway, "server restarting"))
		time.Sleep(20 * time.Millisecond)
		conn.Close()
	})
	defer backend.Close()

	ts, vk := newWSProxyServer(t, "ws"+strings.TrimPrefix(backend.URL, "http"))
	client := dialTestWS(t, ts, "/testchain")
	defer client.Close()

	code := wsCloseCode(client)
	if code != websocket.CloseGoingAway {
		t.Errorf("client expected close 1001, got %d", code)
	}

	<-done
	settle()

	if wsIsUnhealthy(t, vk) {
		t.Error("endpoint must remain healthy after backend 1001 (Going Away)")
	}
}

// TestWSClose_Backend_AbnormalClosure verifies that a TCP drop (no WS close
// frame, gorilla reports 1006) from the backend IS counted as a failure.
// The client must receive 1001 (remapped per RFC 6455) and the endpoint must
// be marked WS-unhealthy.
func TestWSClose_Backend_AbnormalClosure(t *testing.T) {
	done := make(chan struct{})
	backend := newBackendWSServer(func(conn *websocket.Conn) {
		defer close(done)
		// Drop the TCP connection without sending a WS close frame.
		// gorilla on the other side will surface this as CloseAbnormalClosure (1006).
		conn.Close()
	})
	defer backend.Close()

	ts, vk := newWSProxyServer(t, "ws"+strings.TrimPrefix(backend.URL, "http"))
	client := dialTestWS(t, ts, "/testchain")
	defer client.Close()

	// proxyWebSocketCopy remaps 1006 to 1001 (Going Away) because 1006 must not
	// be sent on the wire per RFC 6455.
	code := wsCloseCode(client)
	if code != websocket.CloseGoingAway {
		t.Errorf("client expected close 1001 (remapped from 1006), got %d", code)
	}

	<-done
	settle()

	if !wsIsUnhealthy(t, vk) {
		t.Error("endpoint must be marked WS-unhealthy after backend TCP drop (1006)")
	}
}

// TestWSClose_Backend_InternalError verifies that a 1011 close from the backend
// IS counted as a failure, since it signals an unexpected server error.
// The client must receive 1011 and the endpoint must be marked WS-unhealthy.
func TestWSClose_Backend_InternalError(t *testing.T) {
	done := make(chan struct{})
	backend := newBackendWSServer(func(conn *websocket.Conn) {
		defer close(done)
		conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "internal error"))
		time.Sleep(20 * time.Millisecond)
		conn.Close()
	})
	defer backend.Close()

	ts, vk := newWSProxyServer(t, "ws"+strings.TrimPrefix(backend.URL, "http"))
	client := dialTestWS(t, ts, "/testchain")
	defer client.Close()

	code := wsCloseCode(client)
	if code != websocket.CloseInternalServerErr {
		t.Errorf("client expected close 1011, got %d", code)
	}

	<-done
	settle()

	if !wsIsUnhealthy(t, vk) {
		t.Error("endpoint must be marked WS-unhealthy after backend 1011 (Internal Server Error)")
	}
}

// --- Client-initiated closures ---

// TestWSClose_Client_NormalClosure verifies that a client-initiated 1000 closure
// does not count against the backend's health.
func TestWSClose_Client_NormalClosure(t *testing.T) {
	ready := make(chan struct{})
	done := make(chan struct{})
	backend := newBackendWSServer(func(conn *websocket.Conn) {
		defer close(done)
		close(ready)
		conn.ReadMessage() // blocks until client closes
		conn.Close()
	})
	defer backend.Close()

	ts, vk := newWSProxyServer(t, "ws"+strings.TrimPrefix(backend.URL, "http"))
	client := dialTestWS(t, ts, "/testchain")

	<-ready
	client.WriteMessage(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	client.Close()

	<-done
	settle()

	if wsIsUnhealthy(t, vk) {
		t.Error("endpoint must remain healthy after client-initiated 1000 close")
	}
}

// TestWSClose_Client_AbnormalClosure verifies that a client TCP drop (1006 from
// the client side) does not count against the backend's health.
func TestWSClose_Client_AbnormalClosure(t *testing.T) {
	ready := make(chan struct{})
	done := make(chan struct{})
	backend := newBackendWSServer(func(conn *websocket.Conn) {
		defer close(done)
		close(ready)
		conn.ReadMessage()
		conn.Close()
	})
	defer backend.Close()

	ts, vk := newWSProxyServer(t, "ws"+strings.TrimPrefix(backend.URL, "http"))
	client := dialTestWS(t, ts, "/testchain")

	<-ready
	// Drop TCP without WS close frame, aetherlay sees this as a client-side 1006.
	client.Close()

	<-done
	settle()

	if wsIsUnhealthy(t, vk) {
		t.Error("endpoint must remain healthy after client TCP drop")
	}
}

// TestWSClose_Client_GoingAway verifies that a client-initiated 1001 close does
// not count against the backend's health.
func TestWSClose_Client_GoingAway(t *testing.T) {
	ready := make(chan struct{})
	done := make(chan struct{})
	backend := newBackendWSServer(func(conn *websocket.Conn) {
		defer close(done)
		close(ready)
		conn.ReadMessage()
		conn.Close()
	})
	defer backend.Close()

	ts, vk := newWSProxyServer(t, "ws"+strings.TrimPrefix(backend.URL, "http"))
	client := dialTestWS(t, ts, "/testchain")

	<-ready
	client.WriteMessage(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseGoingAway, "client shutting down"))
	client.Close()

	<-done
	settle()

	if wsIsUnhealthy(t, vk) {
		t.Error("endpoint must remain healthy after client-initiated 1001 close")
	}
}
