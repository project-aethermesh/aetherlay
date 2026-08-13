package health

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"aetherlay/internal/config"
	"aetherlay/internal/store"
)

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

func TestCheckHTTPHealthGuardKeepsUnhealthyOnPassingProbe(t *testing.T) {
	server := solanaRPCTestServer(t, 123456, true)
	defer server.Close()

	valkeyClient := store.NewMockValkeyClient()
	valkeyClient.PopulateStatuses(map[string]*store.EndpointStatus{
		"solana-mainnet:test-1": {HasHTTP: true, HealthyHTTP: false, LastHealthCheck: time.Now().Add(-time.Minute)},
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

func TestCheckHTTPHealthGuardFlipsToUnhealthyImmediately(t *testing.T) {
	// Slot 0 is an invalid/unhealthy result parsed by checkHealthParams itself, not an
	// RPC-level error, so it goes through the same write path as a normal healthy
	// result and exercises the guard, rather than one of checkHTTPHealth's early-return
	// branches (a hard RPC error on the block or sync call). Those return before ever
	// calling updateEndpointStatusInValkey, which is a separate, pre-existing gap
	// unrelated to this guard, masked in the real periodic sweep by checkEndpoint's own
	// outer write.
	server := solanaRPCTestServer(t, 0, true)
	defer server.Close()

	valkeyClient := store.NewMockValkeyClient()
	valkeyClient.PopulateStatuses(map[string]*store.EndpointStatus{
		"solana-mainnet:test-1": {HasHTTP: true, HealthyHTTP: true, LastHealthCheck: time.Now().Add(-time.Minute)},
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

func TestCheckHTTPHealthGuardFallbackWhenEphemeralDisabled(t *testing.T) {
	server := solanaRPCTestServer(t, 123456, true)
	defer server.Close()

	valkeyClient := store.NewMockValkeyClient()
	valkeyClient.PopulateStatuses(map[string]*store.EndpointStatus{
		"solana-mainnet:test-1": {HasHTTP: true, HealthyHTTP: false, LastHealthCheck: time.Now().Add(-time.Minute)},
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

func TestCheckEndpointGuardKeepsUnhealthyOnPassingProbe(t *testing.T) {
	valkeyClient := store.NewMockValkeyClient()
	valkeyClient.PopulateStatuses(map[string]*store.EndpointStatus{
		"solana-mainnet:test-1": {HasHTTP: true, HealthyHTTP: false, LastHealthCheck: time.Now().Add(-time.Minute)},
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
			t.Fatalf("failed to decode request: %v", err)
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
