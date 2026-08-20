package health

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aetherlay/internal/config"
	"aetherlay/internal/store"

	"github.com/gorilla/websocket"
)

// TestResolveHealthTransition covers every (hasPriorCheck, currentlyHealthy, probeHealthy)
// combination, including the ephemeral-checks-disabled fallback.
func TestResolveHealthTransition(t *testing.T) {
	tests := []struct {
		name                   string
		ephemeralChecksEnabled bool
		hasPriorCheck          bool
		currentlyHealthy       bool
		probeHealthy           bool
		want                   bool
	}{
		{"first ever check, probe healthy", true, false, false, true, true},
		{"first ever check, probe unhealthy", true, false, false, false, false},
		{"prior check, was healthy, probe healthy", true, true, true, true, true},
		{"prior check, was healthy, probe unhealthy applies immediately", true, true, true, false, false},
		{"prior check, was unhealthy, probe healthy stays unhealthy", true, true, false, true, false},
		{"prior check, was unhealthy, probe unhealthy", true, true, false, false, false},
		{"ephemeral disabled, was unhealthy, probe healthy flips immediately", false, true, false, true, true},
		{"ephemeral disabled, was healthy, probe unhealthy flips immediately", false, true, true, false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Checker{ephemeralChecksEnabled: tt.ephemeralChecksEnabled}
			got := c.resolveHealthTransition(tt.hasPriorCheck, tt.currentlyHealthy, tt.probeHealthy)
			if got != tt.want {
				t.Errorf("resolveHealthTransition(%v, %v, %v) = %v, want %v", tt.hasPriorCheck, tt.currentlyHealthy, tt.probeHealthy, got, tt.want)
			}
		})
	}
}

// TestCheckHTTPHealthGuardKeepsUnhealthyOnPassingProbe verifies that a single passing
// periodic sweep does not flip a previously-unhealthy endpoint back to healthy.
func TestCheckHTTPHealthGuardKeepsUnhealthyOnPassingProbe(t *testing.T) {
	server := solanaRPCTestServer(t, 123456, true)
	defer server.Close()

	valkeyClient := store.NewMockValkeyClient()
	valkeyClient.PopulateStatuses(map[string]*store.EndpointStatus{
		"solana-mainnet:test-1": {HasHTTP: true, HealthyHTTP: false, LastHTTPHealthCheck: time.Now().Add(-time.Minute)},
	})
	checker := &Checker{
		valkeyClient:           valkeyClient,
		healthCheckSyncStatus:  true,
		ephemeralChecksEnabled: true,
	}
	endpoint := config.Endpoint{Provider: "test", ChainType: config.ChainTypeSolana, HTTPURL: server.URL}

	if healthy := checker.checkHTTPHealth(context.Background(), "solana-mainnet", "test-1", endpoint); !healthy {
		t.Error("expected the probe itself to report healthy")
	}

	status, err := valkeyClient.GetEndpointStatus(context.Background(), "solana-mainnet", "test-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status.HealthyHTTP {
		t.Error("expected the periodic sweep NOT to flip a previously-unhealthy endpoint back to healthy on a single passing probe; that's the ephemeral checker's job")
	}
}

// TestCheckHTTPHealthGuardFlipsToUnhealthyImmediately verifies that a failing probe ejects
// a previously-healthy endpoint right away, with no debounce.
func TestCheckHTTPHealthGuardFlipsToUnhealthyImmediately(t *testing.T) {
	// Slot 0 is an invalid/unhealthy result parsed by checkHealthParams itself, not a
	// hard RPC-level error on the block/sync call (see
	// TestCheckHTTPHealthPersistsUnhealthyOnHardSyncCallError below for that path).
	server := solanaRPCTestServer(t, 0, true)
	defer server.Close()

	valkeyClient := store.NewMockValkeyClient()
	valkeyClient.PopulateStatuses(map[string]*store.EndpointStatus{
		"solana-mainnet:test-1": {HasHTTP: true, HealthyHTTP: true, LastHTTPHealthCheck: time.Now().Add(-time.Minute)},
	})
	checker := &Checker{
		valkeyClient:           valkeyClient,
		healthCheckSyncStatus:  true,
		ephemeralChecksEnabled: true,
	}
	endpoint := config.Endpoint{Provider: "test", ChainType: config.ChainTypeSolana, HTTPURL: server.URL}

	checker.checkHTTPHealth(context.Background(), "solana-mainnet", "test-1", endpoint)

	status, err := valkeyClient.GetEndpointStatus(context.Background(), "solana-mainnet", "test-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status.HealthyHTTP {
		t.Error("expected a failing probe to eject a previously-healthy endpoint immediately, with no debounce")
	}
}

// TestCheckHTTPHealthPersistsUnhealthyOnHardBlockCallError covers a hard RPC error on the
// block/slot call itself (a real 5xx from the endpoint, not just an invalid result), which
// used to return before ever calling updateEndpointStatusInValkey, leaving a stale
// HealthyHTTP value in Valkey indefinitely if the endpoint kept failing this way.
func TestCheckHTTPHealthPersistsUnhealthyOnHardBlockCallError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	valkeyClient := store.NewMockValkeyClient()
	valkeyClient.PopulateStatuses(map[string]*store.EndpointStatus{
		"solana-mainnet:test-1": {HasHTTP: true, HealthyHTTP: true, BlockNumber: 42, LastHTTPHealthCheck: time.Now().Add(-time.Minute)},
	})
	checker := &Checker{
		valkeyClient:           valkeyClient,
		healthCheckSyncStatus:  true,
		ephemeralChecksEnabled: true,
	}
	endpoint := config.Endpoint{Provider: "test", ChainType: config.ChainTypeSolana, HTTPURL: server.URL}

	if healthy := checker.checkHTTPHealth(context.Background(), "solana-mainnet", "test-1", endpoint); healthy {
		t.Error("expected a hard error on the block call to report unhealthy")
	}

	status, err := valkeyClient.GetEndpointStatus(context.Background(), "solana-mainnet", "test-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status.HealthyHTTP {
		t.Error("expected a hard error on the block call to persist HealthyHTTP=false, not leave the stale prior value in place")
	}
	if status.BlockNumber != 42 {
		t.Errorf("expected the last known block number to be preserved when the block call itself fails, got %d", status.BlockNumber)
	}
}

// TestCheckHTTPHealthPersistsUnhealthyOnHardSyncCallError is the same as above but for a
// hard (non-method-not-found) JSON-RPC error on the sync/health-status call.
func TestCheckHTTPHealthPersistsUnhealthyOnHardSyncCallError(t *testing.T) {
	server := solanaRPCTestServer(t, 123456, false) // getSlot ok, getHealth returns a real JSON-RPC error
	defer server.Close()

	valkeyClient := store.NewMockValkeyClient()
	valkeyClient.PopulateStatuses(map[string]*store.EndpointStatus{
		"solana-mainnet:test-1": {HasHTTP: true, HealthyHTTP: true, LastHTTPHealthCheck: time.Now().Add(-time.Minute)},
	})
	checker := &Checker{
		valkeyClient:           valkeyClient,
		healthCheckSyncStatus:  true,
		ephemeralChecksEnabled: true,
	}
	endpoint := config.Endpoint{Provider: "test", ChainType: config.ChainTypeSolana, HTTPURL: server.URL}

	if healthy := checker.checkHTTPHealth(context.Background(), "solana-mainnet", "test-1", endpoint); healthy {
		t.Error("expected a hard error on the sync call to report unhealthy")
	}

	status, err := valkeyClient.GetEndpointStatus(context.Background(), "solana-mainnet", "test-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status.HealthyHTTP {
		t.Error("expected a hard error on the sync call to persist HealthyHTTP=false, not leave the stale prior value in place")
	}
}

// TestCheckHTTPHealthGuardFallbackWhenEphemeralDisabled verifies that the old
// unconditional-overwrite behavior is preserved when ephemeral checks are disabled, since
// there's no other recovery path in that case.
func TestCheckHTTPHealthGuardFallbackWhenEphemeralDisabled(t *testing.T) {
	server := solanaRPCTestServer(t, 123456, true)
	defer server.Close()

	valkeyClient := store.NewMockValkeyClient()
	valkeyClient.PopulateStatuses(map[string]*store.EndpointStatus{
		"solana-mainnet:test-1": {HasHTTP: true, HealthyHTTP: false, LastHTTPHealthCheck: time.Now().Add(-time.Minute)},
	})
	checker := &Checker{
		valkeyClient:           valkeyClient,
		healthCheckSyncStatus:  true,
		ephemeralChecksEnabled: false,
	}
	endpoint := config.Endpoint{Provider: "test", ChainType: config.ChainTypeSolana, HTTPURL: server.URL}

	checker.checkHTTPHealth(context.Background(), "solana-mainnet", "test-1", endpoint)

	status, err := valkeyClient.GetEndpointStatus(context.Background(), "solana-mainnet", "test-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !status.HealthyHTTP {
		t.Error("expected the old unconditional-overwrite behavior to be preserved when ephemeral checks are disabled, since there's no other recovery path")
	}
}

// TestCheckHTTPHealthFirstEverCheckBecomesHealthyImmediately verifies that a brand new
// endpoint's very first check can become healthy right away, without waiting on the
// ephemeral checker.
func TestCheckHTTPHealthFirstEverCheckBecomesHealthyImmediately(t *testing.T) {
	server := solanaRPCTestServer(t, 123456, true)
	defer server.Close()

	// No PopulateStatuses call: this endpoint has never been checked before.
	valkeyClient := store.NewMockValkeyClient()
	checker := &Checker{
		valkeyClient:           valkeyClient,
		healthCheckSyncStatus:  true,
		ephemeralChecksEnabled: true,
	}
	endpoint := config.Endpoint{Provider: "test", ChainType: config.ChainTypeSolana, HTTPURL: server.URL}

	checker.checkHTTPHealth(context.Background(), "solana-mainnet", "test-1", endpoint)

	status, err := valkeyClient.GetEndpointStatus(context.Background(), "solana-mainnet", "test-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !status.HealthyHTTP {
		t.Error("expected a brand new endpoint's very first check to be able to become healthy immediately, without waiting on the ephemeral checker")
	}
}

// TestCheckEndpointGuardKeepsUnhealthyOnPassingProbe verifies that checkEndpoint's own
// write site respects the same guard as checkHTTPHealth, instead of re-introducing the raw
// unguarded probe result.
func TestCheckEndpointGuardKeepsUnhealthyOnPassingProbe(t *testing.T) {
	valkeyClient := store.NewMockValkeyClient()
	valkeyClient.PopulateStatuses(map[string]*store.EndpointStatus{
		"solana-mainnet:test-1": {HasHTTP: true, HealthyHTTP: false, LastHTTPHealthCheck: time.Now().Add(-time.Minute)},
	})
	checker := &Checker{
		valkeyClient:           valkeyClient,
		ephemeralChecksEnabled: true,
	}
	checker.CheckHTTPHealthFunc = func(_ context.Context, _, _ string, _ config.Endpoint) bool { return true }
	checker.CheckWSHealthFunc = func(_ context.Context, _, _ string, _ config.Endpoint) bool { return false }

	endpoint := config.Endpoint{Provider: "test", ChainType: config.ChainTypeSolana, HTTPURL: "http://example.invalid"}
	checker.checkEndpoint(context.Background(), "solana-mainnet", "test-1", endpoint)

	status, err := valkeyClient.GetEndpointStatus(context.Background(), "solana-mainnet", "test-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status.HealthyHTTP {
		t.Error("expected checkEndpoint's own write to respect the same guard as checkHTTPHealth, not silently re-introduce the raw unguarded probe result")
	}
}

// solanaGetBlockTestServer extends the getSlot/getHealth fixture with a getBlock handler,
// so custom-probe consumption can be tested against a fully successful round.
func solanaGetBlockTestServer(t *testing.T, slot int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("failed to decode request: %v", err)
			return
		}
		method, _ := req["method"].(string)

		w.Header().Set("Content-Type", "application/json")
		switch method {
		case "getSlot":
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": slot})
		case "getHealth":
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": "ok"})
		case "getBlock":
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"blockHeight": slot}})
		default:
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "error": map[string]any{"code": -32601, "message": "Method not found"}})
		}
	}))
}

// TestCheckHTTPHealthCustomProbeFailureOverridesOtherwiseHealthy verifies that a failing
// custom probe re-test overrides an otherwise-healthy getSlot/getHealth result.
func TestCheckHTTPHealthCustomProbeFailureOverridesOtherwiseHealthy(t *testing.T) {
	// solanaRPCTestServer only understands getSlot/getHealth; any custom probe method
	// (like getBlock) hits its default "method not found" branch, which is a real
	// failure since getBlock isn't in the optional-methods leniency list.
	server := solanaRPCTestServer(t, 123456, true)
	defer server.Close()

	valkeyClient := store.NewMockValkeyClient()
	if err := valkeyClient.SetCustomProbeState(context.Background(), "solana-mainnet", "test-1", store.CustomProbeState{
		Method: "getBlock",
		SetAt:  time.Now(),
	}); err != nil {
		t.Fatalf("failed to seed custom probe state: %v", err)
	}

	checker := &Checker{valkeyClient: valkeyClient, healthCheckSyncStatus: true}
	endpoint := config.Endpoint{Provider: "test", ChainType: config.ChainTypeSolana, HTTPURL: server.URL}

	if healthy := checker.checkHTTPHealth(context.Background(), "solana-mainnet", "test-1", endpoint); healthy {
		t.Error("expected a failing custom probe re-test to override an otherwise-healthy getSlot/getHealth result")
	}
}

// TestCheckHTTPHealthCustomProbeSuccessKeepsHealthy verifies that a passing custom probe
// re-test alongside a healthy default probe reports healthy overall.
func TestCheckHTTPHealthCustomProbeSuccessKeepsHealthy(t *testing.T) {
	server := solanaGetBlockTestServer(t, 123456)
	defer server.Close()

	valkeyClient := store.NewMockValkeyClient()
	if err := valkeyClient.SetCustomProbeState(context.Background(), "solana-mainnet", "test-1", store.CustomProbeState{
		Method: "getBlock",
		SetAt:  time.Now(),
	}); err != nil {
		t.Fatalf("failed to seed custom probe state: %v", err)
	}

	checker := &Checker{valkeyClient: valkeyClient, healthCheckSyncStatus: true}
	endpoint := config.Endpoint{Provider: "test", ChainType: config.ChainTypeSolana, HTTPURL: server.URL}

	if healthy := checker.checkHTTPHealth(context.Background(), "solana-mainnet", "test-1", endpoint); !healthy {
		t.Error("expected a passing custom probe re-test alongside a healthy default probe to report healthy")
	}
}

// TestCheckHTTPHealthCustomProbeSlotSkippedTreatedAsInconclusive verifies that a getBlock
// re-test failing with a skipped-slot error code (-32007/-32009) does not flip an
// otherwise-healthy endpoint to unhealthy, since that indicates the target slot has no
// block rather than that the endpoint can't serve getBlock.
func TestCheckHTTPHealthCustomProbeSlotSkippedTreatedAsInconclusive(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("failed to decode request: %v", err)
			return
		}
		method, _ := req["method"].(string)

		w.Header().Set("Content-Type", "application/json")
		switch method {
		case "getSlot":
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": 123456})
		case "getHealth":
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": "ok"})
		case "getBlock":
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "error": map[string]any{"code": -32009, "message": "Slot 123424 was skipped, or missing in long-term storage"}})
		default:
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "error": map[string]any{"code": -32601, "message": "Method not found"}})
		}
	}))
	defer server.Close()

	valkeyClient := store.NewMockValkeyClient()
	if err := valkeyClient.SetCustomProbeState(context.Background(), "solana-mainnet", "test-1", store.CustomProbeState{
		Method: "getBlock",
		SetAt:  time.Now(),
	}); err != nil {
		t.Fatalf("failed to seed custom probe state: %v", err)
	}

	checker := &Checker{valkeyClient: valkeyClient, healthCheckSyncStatus: true}
	endpoint := config.Endpoint{Provider: "test", ChainType: config.ChainTypeSolana, HTTPURL: server.URL}

	if healthy := checker.checkHTTPHealth(context.Background(), "solana-mainnet", "test-1", endpoint); !healthy {
		t.Error("expected a skipped-slot custom probe response to be treated as inconclusive, not a failure")
	}
}

// TestRunEphemeralCheckProtocolClearsCustomProbeStateOnRecovery verifies that reaching the
// ephemeral recovery threshold clears any active custom probe state.
func TestRunEphemeralCheckProtocolClearsCustomProbeStateOnRecovery(t *testing.T) {
	valkeyClient := store.NewMockValkeyClient()
	valkeyClient.PopulateStatuses(map[string]*store.EndpointStatus{
		"solana-devnet:ep1": {HasHTTP: true, HealthyHTTP: false},
	})
	if err := valkeyClient.SetCustomProbeState(context.Background(), "solana-devnet", "ep1", store.CustomProbeState{
		Method: "getBlock",
		SetAt:  time.Now(),
	}); err != nil {
		t.Fatalf("failed to seed custom probe state: %v", err)
	}

	checker := &Checker{valkeyClient: valkeyClient}
	checker.CheckHTTPHealthFunc = func(_ context.Context, _, _ string, _ config.Endpoint) bool { return true }

	checker.runEphemeralCheckProtocol(context.Background(), "solana-devnet", "ep1", config.Endpoint{}, time.Millisecond, 1, "solana-devnet|ep1|http", "http")

	status, err := valkeyClient.GetEndpointStatus(context.Background(), "solana-devnet", "ep1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !status.HealthyHTTP {
		t.Error("expected the endpoint to be marked healthy after reaching the ephemeral threshold")
	}

	state, err := valkeyClient.GetCustomProbeState(context.Background(), "solana-devnet", "ep1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state != nil {
		t.Errorf("expected custom probe state to be cleared on confirmed recovery, got %+v", state)
	}
}

// solanaWSTestServer mirrors solanaRPCTestServer but over a WebSocket connection, so
// checkWSHealth can be exercised end-to-end.
func solanaWSTestServer(t *testing.T, slot int64, healthy bool) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("failed to upgrade connection: %v", err)
			return
		}
		defer conn.Close()

		var req map[string]any
		if err := conn.ReadJSON(&req); err != nil {
			return
		}
		method, _ := req["method"].(string)

		var resp map[string]any
		switch method {
		case "getSlot":
			resp = map[string]any{"jsonrpc": "2.0", "id": 1, "result": slot}
		case "getHealth":
			if healthy {
				resp = map[string]any{"jsonrpc": "2.0", "id": 1, "result": "ok"}
			} else {
				resp = map[string]any{"jsonrpc": "2.0", "id": 1, "error": map[string]any{"code": -32005, "message": "Node is unhealthy"}}
			}
		default:
			resp = map[string]any{"jsonrpc": "2.0", "id": 1, "error": map[string]any{"code": -32601, "message": "Method not found"}}
		}
		conn.WriteJSON(resp)
	}))
}

// TestCheckWSHealthTracksPriorCheckSeparatelyFromHTTP is a regression guard: HTTP and WS
// prior-check state must not share a single timestamp. StartEphemeralChecks' own startup
// sweep checks HTTP before WS for a given endpoint; if both protocols shared one marker,
// WS's own first-ever check would look like a prior observation (because HTTP had just
// set it) and get stuck unhealthy on a passing probe instead of being accepted right away.
func TestCheckWSHealthTracksPriorCheckSeparatelyFromHTTP(t *testing.T) {
	server := solanaWSTestServer(t, 123456, true)
	defer server.Close()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")

	valkeyClient := store.NewMockValkeyClient()
	// HTTP has already been checked (LastHTTPHealthCheck set); WS never has.
	valkeyClient.PopulateStatuses(map[string]*store.EndpointStatus{
		"solana-mainnet:test-1": {HasWS: true, HealthyWS: false, LastHTTPHealthCheck: time.Now()},
	})
	checker := &Checker{
		valkeyClient:           valkeyClient,
		healthCheckSyncStatus:  true,
		ephemeralChecksEnabled: true,
	}
	endpoint := config.Endpoint{Provider: "test", ChainType: config.ChainTypeSolana, WSURL: wsURL}

	if healthy := checker.checkWSHealth(context.Background(), "solana-mainnet", "test-1", endpoint); !healthy {
		t.Error("expected the probe itself to report healthy")
	}

	status, err := valkeyClient.GetEndpointStatus(context.Background(), "solana-mainnet", "test-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !status.HealthyWS {
		t.Error("expected WS's own first-ever check to be accepted immediately, not gated because HTTP had already been checked")
	}
}
