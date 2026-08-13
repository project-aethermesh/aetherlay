package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"aetherlay/internal/config"
	"aetherlay/internal/store"
)

func newCustomProbeTestServer(chain, endpointID string) (*Server, *store.MockValkeyClient) {
	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			chain: {
				endpointID: config.Endpoint{Provider: "test", ChainType: config.ChainTypeSolana, HTTPURL: "http://fail", Role: "primary", Type: "full"},
			},
		},
	}
	valkeyClient := store.NewMockValkeyClient()
	valkeyClient.PopulateStatuses(map[string]*store.EndpointStatus{
		chain + ":" + endpointID: {HasHTTP: true, HealthyHTTP: true},
	})
	server := NewServer(cfg, valkeyClient, createTestConfig())
	return server, valkeyClient
}

func TestMaybeSetCustomProbeMethodSetsAllowlistedMethod(t *testing.T) {
	server, valkeyClient := newCustomProbeTestServer("solana-devnet", "ep1")

	body := []byte(`{"jsonrpc":"2.0","method":"getBlock","params":[123],"id":1}`)
	server.maybeSetCustomProbeMethod("solana-devnet", "ep1", body)

	state, err := valkeyClient.GetCustomProbeState(context.Background(), "solana-devnet", "ep1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state == nil {
		t.Fatal("expected a custom probe state to be set")
	}
	if state.Method != "getBlock" {
		t.Errorf("expected method getBlock, got %q", state.Method)
	}
}

func TestMaybeSetCustomProbeMethodIgnoresNonAllowlistedMethod(t *testing.T) {
	server, valkeyClient := newCustomProbeTestServer("solana-devnet", "ep1")

	// sendTransaction is state-mutating and must never be captured for replay.
	body := []byte(`{"jsonrpc":"2.0","method":"sendTransaction","params":["deadbeef"],"id":1}`)
	server.maybeSetCustomProbeMethod("solana-devnet", "ep1", body)

	state, err := valkeyClient.GetCustomProbeState(context.Background(), "solana-devnet", "ep1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state != nil {
		t.Errorf("expected no custom probe state for a non-allowlisted method, got %+v", state)
	}
}

// TestMaybeSetCustomProbeMethodIgnoresMethodForWrongChainType is a regression guard:
// eth_getBlockByNumber is a real, allowlisted method name, but not for a Solana endpoint.
// Capturing it there would later have the health checker replay an EVM-shaped request
// against a Solana node, which would just fail.
func TestMaybeSetCustomProbeMethodIgnoresMethodForWrongChainType(t *testing.T) {
	server, valkeyClient := newCustomProbeTestServer("solana-devnet", "ep1") // Solana endpoint

	body := []byte(`{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["latest",false],"id":1}`)
	server.maybeSetCustomProbeMethod("solana-devnet", "ep1", body)

	state, err := valkeyClient.GetCustomProbeState(context.Background(), "solana-devnet", "ep1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state != nil {
		t.Errorf("expected no custom probe state for a method that belongs to a different chain type, got %+v", state)
	}
}

// TestMaybeSetCustomProbeMethodIgnoresGetBlockOnEVMEndpoint is the reverse case: getBlock
// is a real Solana method name, but must not be captured for an EVM endpoint.
func TestMaybeSetCustomProbeMethodIgnoresGetBlockOnEVMEndpoint(t *testing.T) {
	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"ethereum": {
				"ep1": config.Endpoint{Provider: "test", ChainType: config.ChainTypeEVM, HTTPURL: "http://fail", Role: "primary", Type: "full"},
			},
		},
	}
	valkeyClient := store.NewMockValkeyClient()
	valkeyClient.PopulateStatuses(map[string]*store.EndpointStatus{
		"ethereum:ep1": {HasHTTP: true, HealthyHTTP: true},
	})
	server := NewServer(cfg, valkeyClient, createTestConfig())

	body := []byte(`{"jsonrpc":"2.0","method":"getBlock","params":[123],"id":1}`)
	server.maybeSetCustomProbeMethod("ethereum", "ep1", body)

	state, err := valkeyClient.GetCustomProbeState(context.Background(), "ethereum", "ep1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state != nil {
		t.Errorf("expected no custom probe state for getBlock on an EVM endpoint, got %+v", state)
	}
}

func TestMaybeSetCustomProbeMethodIgnoresUnparseableBody(t *testing.T) {
	server, valkeyClient := newCustomProbeTestServer("solana-devnet", "ep1")

	// A batch (array) JSON-RPC request doesn't unmarshal into the single-object shape
	// extractRPCMethod expects; the safe default is to skip capture.
	body := []byte(`[{"jsonrpc":"2.0","method":"getBlock","params":[1],"id":1}]`)
	server.maybeSetCustomProbeMethod("solana-devnet", "ep1", body)

	state, err := valkeyClient.GetCustomProbeState(context.Background(), "solana-devnet", "ep1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state != nil {
		t.Errorf("expected no custom probe state for an unparseable/batch body, got %+v", state)
	}
}

func TestMaybeSetCustomProbeMethodDoesNotOverwriteWithinRefreshPeriod(t *testing.T) {
	server, valkeyClient := newCustomProbeTestServer("solana-devnet", "ep1")

	seededAt := time.Now()
	if err := valkeyClient.SetCustomProbeState(context.Background(), "solana-devnet", "ep1", store.CustomProbeState{
		Method: "getBlock",
		SetAt:  seededAt,
	}); err != nil {
		t.Fatalf("failed to seed custom probe state: %v", err)
	}

	// Same method failing again shortly after: the capture is still gated by the refresh
	// period, so SetAt must stay exactly as seeded rather than being bumped forward.
	body := []byte(`{"jsonrpc":"2.0","method":"getBlock","params":[123],"id":1}`)
	server.maybeSetCustomProbeMethod("solana-devnet", "ep1", body)

	state, err := valkeyClient.GetCustomProbeState(context.Background(), "solana-devnet", "ep1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state == nil || !state.SetAt.Equal(seededAt) {
		t.Errorf("expected SetAt to stay stable within the refresh period, got %+v (seeded at %v)", state, seededAt)
	}
}

func TestMaybeSetCustomProbeMethodOverwritesAfterRefreshPeriodElapses(t *testing.T) {
	server, valkeyClient := newCustomProbeTestServer("solana-devnet", "ep1")

	oldSetAt := time.Now().Add(-2 * server.customProbeRefreshPeriod)
	if err := valkeyClient.SetCustomProbeState(context.Background(), "solana-devnet", "ep1", store.CustomProbeState{
		Method: "getBlock",
		SetAt:  oldSetAt,
	}); err != nil {
		t.Fatalf("failed to seed custom probe state: %v", err)
	}

	body := []byte(`{"jsonrpc":"2.0","method":"getBlock","params":[456],"id":1}`)
	server.maybeSetCustomProbeMethod("solana-devnet", "ep1", body)

	state, err := valkeyClient.GetCustomProbeState(context.Background(), "solana-devnet", "ep1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state == nil || state.SetAt.Equal(oldSetAt) || time.Since(state.SetAt) > time.Second {
		t.Errorf("expected SetAt to refresh to now once the refresh period elapsed, got %+v (old was %v)", state, oldSetAt)
	}
}

// TestForwardRequestCapturesCustomProbeMethodOn5xx is an integration-style check that the
// capture is actually wired into the live proxy path, not just directly callable.
func TestForwardRequestCapturesCustomProbeMethodOn5xx(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":19,"message":"Temporary internal error"}}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"solana-devnet": {
				"ep1": config.Endpoint{Provider: "test", ChainType: config.ChainTypeSolana, HTTPURL: upstream.URL, Role: "primary", Type: "full"},
			},
		},
	}
	valkeyClient := store.NewMockValkeyClient()
	valkeyClient.PopulateStatuses(map[string]*store.EndpointStatus{
		"solana-devnet:ep1": {HasHTTP: true, HealthyHTTP: true},
	})
	server := NewServer(cfg, valkeyClient, createTestConfig())

	body := []byte(`{"jsonrpc":"2.0","method":"getBlock","params":[123],"id":1}`)
	err := server.defaultForwardRequestWithBodyFunc(httptest.NewRecorder(), context.Background(), "POST", upstream.URL, body, http.Header{})
	if err == nil {
		t.Fatal("expected an error from the 500 response")
	}

	state, err := valkeyClient.GetCustomProbeState(context.Background(), "solana-devnet", "ep1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state == nil || state.Method != "getBlock" {
		t.Errorf("expected a real 500 on getBlock to capture it as the custom probe method, got %+v", state)
	}
}

// TestForwardRequestDoesNotCaptureOn400 ensures the capture only fires for real 5xx
// responses, matching the existing "defer to caller" handling for 400s.
func TestForwardRequestDoesNotCaptureOn400(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"solana-devnet": {
				"ep1": config.Endpoint{Provider: "test", ChainType: config.ChainTypeSolana, HTTPURL: upstream.URL, Role: "primary", Type: "full"},
			},
		},
	}
	valkeyClient := store.NewMockValkeyClient()
	valkeyClient.PopulateStatuses(map[string]*store.EndpointStatus{
		"solana-devnet:ep1": {HasHTTP: true, HealthyHTTP: true},
	})
	server := NewServer(cfg, valkeyClient, createTestConfig())

	body := []byte(`{"jsonrpc":"2.0","method":"getBlock","params":[123],"id":1}`)
	_ = server.defaultForwardRequestWithBodyFunc(httptest.NewRecorder(), context.Background(), "POST", upstream.URL, body, http.Header{})

	state, err := valkeyClient.GetCustomProbeState(context.Background(), "solana-devnet", "ep1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state != nil {
		t.Errorf("expected no custom probe state to be captured for a 400 response, got %+v", state)
	}
}
