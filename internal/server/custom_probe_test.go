package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"aetherlay/internal/config"
	"aetherlay/internal/helpers"
	"aetherlay/internal/store"
)

// newCustomProbeTestServer builds a Server with a single healthy Solana endpoint, for
// tests exercising maybeSetCustomProbeMethod in isolation.
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

// TestMaybeSetCustomProbeMethodSetsAllowlistedMethod verifies that a 5xx on an allowlisted
// method captures it as the endpoint's custom probe method.
func TestMaybeSetCustomProbeMethodSetsAllowlistedMethod(t *testing.T) {
	server, valkeyClient := newCustomProbeTestServer("solana-devnet", "ep1")

	body := []byte(`{"jsonrpc":"2.0","method":"getBlock","params":[123],"id":1}`)
	server.maybeSetCustomProbeMethod(context.Background(), "solana-devnet", "ep1", body)

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

// TestMaybeSetCustomProbeMethodIgnoresNonAllowlistedMethod verifies that a state-mutating
// method like sendTransaction is never captured for replay, even on a real 5xx.
func TestMaybeSetCustomProbeMethodIgnoresNonAllowlistedMethod(t *testing.T) {
	server, valkeyClient := newCustomProbeTestServer("solana-devnet", "ep1")

	// sendTransaction is state-mutating and must never be captured for replay.
	body := []byte(`{"jsonrpc":"2.0","method":"sendTransaction","params":["deadbeef"],"id":1}`)
	server.maybeSetCustomProbeMethod(context.Background(), "solana-devnet", "ep1", body)

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
	server.maybeSetCustomProbeMethod(context.Background(), "solana-devnet", "ep1", body)

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
	server.maybeSetCustomProbeMethod(context.Background(), "ethereum", "ep1", body)

	state, err := valkeyClient.GetCustomProbeState(context.Background(), "ethereum", "ep1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state != nil {
		t.Errorf("expected no custom probe state for getBlock on an EVM endpoint, got %+v", state)
	}
}

// TestMaybeSetCustomProbeMethodIgnoresUnparseableBody verifies that a body that doesn't
// unmarshal into the expected single-object shape is skipped rather than erroring.
func TestMaybeSetCustomProbeMethodIgnoresUnparseableBody(t *testing.T) {
	server, valkeyClient := newCustomProbeTestServer("solana-devnet", "ep1")

	// A batch (array) JSON-RPC request doesn't unmarshal into the single-object shape
	// extractRPCMethod expects; the safe default is to skip capture.
	body := []byte(`[{"jsonrpc":"2.0","method":"getBlock","params":[1],"id":1}]`)
	server.maybeSetCustomProbeMethod(context.Background(), "solana-devnet", "ep1", body)

	state, err := valkeyClient.GetCustomProbeState(context.Background(), "solana-devnet", "ep1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state != nil {
		t.Errorf("expected no custom probe state for an unparseable/batch body, got %+v", state)
	}
}

// TestMaybeSetCustomProbeMethodDoesNotOverwriteWithinRefreshPeriod verifies that a second
// failure on the same method within the refresh period is a no-op, since the gate acquired
// by the first call is still held.
func TestMaybeSetCustomProbeMethodDoesNotOverwriteWithinRefreshPeriod(t *testing.T) {
	server, valkeyClient := newCustomProbeTestServer("solana-devnet", "ep1")

	body := []byte(`{"jsonrpc":"2.0","method":"getBlock","params":[123],"id":1}`)
	server.maybeSetCustomProbeMethod(context.Background(), "solana-devnet", "ep1", body)

	firstState, err := valkeyClient.GetCustomProbeState(context.Background(), "solana-devnet", "ep1")
	if err != nil || firstState == nil {
		t.Fatalf("expected an initial custom probe state to be set, got %+v, err=%v", firstState, err)
	}

	// Same method failing again shortly after: the gate acquired by the first call is
	// still held, so this second call must be a no-op rather than bumping SetAt forward.
	server.maybeSetCustomProbeMethod(context.Background(), "solana-devnet", "ep1", body)

	state, err := valkeyClient.GetCustomProbeState(context.Background(), "solana-devnet", "ep1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state == nil || !state.SetAt.Equal(firstState.SetAt) {
		t.Errorf("expected SetAt to stay stable within the refresh period, got %+v (first was %+v)", state, firstState)
	}
}

// TestMaybeSetCustomProbeMethodOverwritesAfterRefreshPeriodElapses verifies that once the
// refresh period elapses, the gate expires and a fresh failure refreshes SetAt.
func TestMaybeSetCustomProbeMethodOverwritesAfterRefreshPeriodElapses(t *testing.T) {
	server, valkeyClient := newCustomProbeTestServer("solana-devnet", "ep1")

	base := time.Now()
	valkeyClient.NowFunc = func() time.Time { return base }

	body := []byte(`{"jsonrpc":"2.0","method":"getBlock","params":[123],"id":1}`)
	server.maybeSetCustomProbeMethod(context.Background(), "solana-devnet", "ep1", body)

	firstState, err := valkeyClient.GetCustomProbeState(context.Background(), "solana-devnet", "ep1")
	if err != nil || firstState == nil {
		t.Fatalf("expected an initial custom probe state to be set, got %+v, err=%v", firstState, err)
	}

	// Fast-forward the mock's clock past the refresh period so the gate looks expired,
	// without a real sleep (see MockValkeyClient.NowFunc).
	valkeyClient.NowFunc = func() time.Time { return base.Add(2 * server.customProbeRefreshPeriod) }

	server.maybeSetCustomProbeMethod(context.Background(), "solana-devnet", "ep1", body)

	state, err := valkeyClient.GetCustomProbeState(context.Background(), "solana-devnet", "ep1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state == nil || !state.SetAt.After(firstState.SetAt) {
		t.Errorf("expected SetAt to refresh once the gate's refresh period elapsed, got %+v (first was %+v)", state, firstState)
	}
}

// TestMaybeSetCustomProbeMethodNoopWhenEphemeralChecksDisabled verifies that capture is
// skipped entirely when ephemeral checks are disabled, since runEphemeralCheckProtocol,
// the only path that ever clears a captured state, never runs in that case, and a
// captured target would otherwise stay pinned forever.
func TestMaybeSetCustomProbeMethodNoopWhenEphemeralChecksDisabled(t *testing.T) {
	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"solana-devnet": {
				"ep1": config.Endpoint{Provider: "test", ChainType: config.ChainTypeSolana, HTTPURL: "http://fail", Role: "primary", Type: "full"},
			},
		},
	}
	valkeyClient := store.NewMockValkeyClient()
	valkeyClient.PopulateStatuses(map[string]*store.EndpointStatus{
		"solana-devnet:ep1": {HasHTTP: true, HealthyHTTP: true},
	})
	appConfig := &helpers.LoadedConfig{
		EphemeralChecksEnabled:   false,
		EndpointFailureThreshold: 1,
		EndpointSuccessThreshold: 1,
		ProxyMaxRetries:          3,
		ProxyTimeout:             15,
		ProxyTimeoutPerTry:       5,
	}
	server := NewServer(cfg, valkeyClient, appConfig)

	body := []byte(`{"jsonrpc":"2.0","method":"getBlock","params":[123],"id":1}`)
	server.maybeSetCustomProbeMethod(context.Background(), "solana-devnet", "ep1", body)

	state, err := valkeyClient.GetCustomProbeState(context.Background(), "solana-devnet", "ep1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state != nil {
		t.Errorf("expected no custom probe state to be captured when ephemeral checks are disabled, got %+v", state)
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
