package health

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aetherlay/internal/config"
	"aetherlay/internal/store"
)

// Gas values observed on a real chain. The baseline is the EVM's intrinsic cost for an
// empty transfer; the precompile leg adds only call overhead on a healthy node.
const (
	gasEmptyTransfer   = "0x5208"    // 21000
	gasEmptyPrecompile = "0x5c85"    // 23685
	gasCannedConstant  = "0x1baf56b" // 29029739, returned by a node that is not executing the call
)

// evmRPCTestServer simulates a minimal EVM JSON-RPC endpoint. estimateGas answers are
// keyed by the destination address so a test can make the precompile leg misbehave while
// the codeless leg stays correct, which is exactly how the real failure presented.
func evmRPCTestServer(t *testing.T, gasByTo map[string]string, estimateGasError bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
			Params []struct {
				To string `json:"to"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("failed to decode request: %v", err)
		}

		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)

		switch req.Method {
		case methodEVMBlockNumber:
			enc.Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": "0x2e00000"})
		case methodEVMSyncing:
			enc.Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": false})
		case methodEVMEstimateGas:
			if estimateGasError {
				enc.Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "error": map[string]any{"code": -32601, "message": "the method eth_estimateGas is not supported"}})
				return
			}
			if len(req.Params) != 1 {
				t.Fatalf("expected exactly one estimateGas param, got %d", len(req.Params))
			}
			gas, ok := gasByTo[strings.ToLower(req.Params[0].To)]
			if !ok {
				t.Fatalf("estimateGas called with an unexpected destination: %s", req.Params[0].To)
			}
			enc.Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": gas})
		default:
			enc.Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "error": map[string]any{"code": -32601, "message": "Method not found"}})
		}
	}))
}

func newExecutionChecker() *Checker {
	return &Checker{
		valkeyClient:          store.NewMockValkeyClient(),
		healthCheckSyncStatus: true,
		healthCheckExecution:  true,
		executionMaxRatio:     10,
	}
}

func TestParseHexQuantity(t *testing.T) {
	cases := []struct {
		name  string
		input any
		want  int64
		ok    bool
	}{
		{"intrinsic transfer cost", "0x5208", 21000, true},
		// The canned constant is a well-formed quantity. Parsing accepts it; the ratio
		// check is what rejects it. Keeping both assertions keeps the two distinct.
		{"canned constant", gasCannedConstant, 29029739, true},
		{"zero is not a plausible estimate", "0x0", 0, false},
		{"missing 0x prefix", "5208", 0, false},
		{"not hex", "0xzz", 0, false},
		{"not a string", float64(21000), 0, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseHexQuantity(tc.input)
			if ok != tc.ok || got != tc.want {
				t.Errorf("ParseHexQuantity(%v) = (%d, %v), want (%d, %v)", tc.input, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestCheckExecutionHealthAcceptsSelfConsistentEstimates(t *testing.T) {
	server := evmRPCTestServer(t, map[string]string{
		probeAddrNoCode:     gasEmptyTransfer,
		probeAddrPrecompile: gasEmptyPrecompile,
	}, false)
	defer server.Close()

	healthy, skipped := newExecutionChecker().checkExecutionHealth(
		context.Background(), "eth-mainnet", "test-1",
		config.Endpoint{Provider: "test", ChainType: config.ChainTypeEVM, HTTPURL: server.URL},
	)

	if skipped {
		t.Fatal("a node that answers eth_estimateGas should be checked, not skipped")
	}
	if !healthy {
		t.Error("estimates within the allowed ratio should report healthy")
	}
}

// The regression this check exists for: the node reports the correct intrinsic cost for a
// codeless destination, so it looks fine, but answers any call that touches code with a
// fixed constant three orders of magnitude too large.
func TestCheckExecutionHealthRejectsCannedGasConstant(t *testing.T) {
	server := evmRPCTestServer(t, map[string]string{
		probeAddrNoCode:     gasEmptyTransfer,
		probeAddrPrecompile: gasCannedConstant,
	}, false)
	defer server.Close()

	healthy, skipped := newExecutionChecker().checkExecutionHealth(
		context.Background(), "0g-galileo", "test-1",
		config.Endpoint{Provider: "test", ChainType: config.ChainTypeEVM, HTTPURL: server.URL},
	)

	if skipped {
		t.Fatal("a node that answers eth_estimateGas should be checked, not skipped")
	}
	if healthy {
		t.Error("an implausible gas estimate should report unhealthy")
	}
}

// Chains that inflate estimates to cover data-availability costs inflate both legs, so
// the ratio stays near 1 and the check must not fire on them.
func TestCheckExecutionHealthAcceptsChainsThatInflateGas(t *testing.T) {
	server := evmRPCTestServer(t, map[string]string{
		probeAddrNoCode:     "0x53340", // 341824
		probeAddrPrecompile: "0x5343f", // 342079
	}, false)
	defer server.Close()

	healthy, _ := newExecutionChecker().checkExecutionHealth(
		context.Background(), "arbitrum", "test-1",
		config.Endpoint{Provider: "test", ChainType: config.ChainTypeEVM, HTTPURL: server.URL},
	)

	if !healthy {
		t.Error("a chain that inflates both legs equally should report healthy")
	}
}

func TestCheckExecutionHealthSkipsEndpointsThatRefuseTheMethod(t *testing.T) {
	server := evmRPCTestServer(t, nil, true)
	defer server.Close()

	healthy, skipped := newExecutionChecker().checkExecutionHealth(
		context.Background(), "eth-mainnet", "test-1",
		config.Endpoint{Provider: "test", ChainType: config.ChainTypeEVM, HTTPURL: server.URL},
	)

	if !skipped {
		t.Fatal("an endpoint that does not serve eth_estimateGas should be skipped")
	}
	if !healthy {
		t.Error("a skipped endpoint must not be marked unhealthy for a capability it never advertised")
	}
}

func TestCheckExecutionHealthRejectsMalformedEstimate(t *testing.T) {
	server := evmRPCTestServer(t, map[string]string{
		probeAddrNoCode:     gasEmptyTransfer,
		probeAddrPrecompile: "not-a-quantity",
	}, false)
	defer server.Close()

	healthy, skipped := newExecutionChecker().checkExecutionHealth(
		context.Background(), "eth-mainnet", "test-1",
		config.Endpoint{Provider: "test", ChainType: config.ChainTypeEVM, HTTPURL: server.URL},
	)

	if skipped {
		t.Fatal("a malformed result is a failure, not a reason to skip")
	}
	if healthy {
		t.Error("a malformed gas estimate should report unhealthy")
	}
}

func TestExecutionCheckApplies(t *testing.T) {
	evm := config.Endpoint{ChainType: config.ChainTypeEVM}

	cases := []struct {
		name     string
		checker  *Checker
		endpoint config.Endpoint
		want     bool
	}{
		{"enabled for EVM", newExecutionChecker(), evm, true},
		{"globally disabled", &Checker{healthCheckExecution: false, executionMaxRatio: 10}, evm, false},
		{"ratio of zero disables it", &Checker{healthCheckExecution: true, executionMaxRatio: 0}, evm, false},
		{"opted out per endpoint", newExecutionChecker(), config.Endpoint{ChainType: config.ChainTypeEVM, SkipExecutionCheck: true}, false},
		{"Solana has no equivalent probe", newExecutionChecker(), config.Endpoint{ChainType: config.ChainTypeSolana}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.checker.executionCheckApplies(tc.endpoint); got != tc.want {
				t.Errorf("executionCheckApplies() = %v, want %v", got, tc.want)
			}
		})
	}
}

// End to end through checkHTTPHealth: the endpoint is at the chain tip and not syncing, so
// every liveness signal passes. Before this check existed it was routed to as healthy.
func TestCheckHTTPHealthRejectsLiveEndpointThatMisreportsGas(t *testing.T) {
	server := evmRPCTestServer(t, map[string]string{
		probeAddrNoCode:     gasEmptyTransfer,
		probeAddrPrecompile: gasCannedConstant,
	}, false)
	defer server.Close()

	endpoint := config.Endpoint{Provider: "test", ChainType: config.ChainTypeEVM, HTTPURL: server.URL}

	if healthy := newExecutionChecker().checkHTTPHealth(context.Background(), "0g-galileo", "test-1", endpoint); healthy {
		t.Error("an endpoint that is live but misreports gas should be marked unhealthy")
	}

	disabled := newExecutionChecker()
	disabled.healthCheckExecution = false
	if healthy := disabled.checkHTTPHealth(context.Background(), "0g-galileo", "test-1", endpoint); !healthy {
		t.Error("with the execution check disabled the endpoint should pass on liveness alone")
	}
}

// A node returning a near-max quantity must not slip past the ratio check by overflowing
// the comparison.
func TestCheckExecutionHealthRejectsOverflowingEstimate(t *testing.T) {
	server := evmRPCTestServer(t, map[string]string{
		probeAddrNoCode:     gasEmptyTransfer,
		probeAddrPrecompile: "0x7fffffffffffffff",
	}, false)
	defer server.Close()

	healthy, skipped := newExecutionChecker().checkExecutionHealth(
		context.Background(), "eth-mainnet", "test-1",
		config.Endpoint{Provider: "test", ChainType: config.ChainTypeEVM, HTTPURL: server.URL},
	)

	if skipped {
		t.Fatal("a parseable result is a check, not a skip")
	}
	if healthy {
		t.Error("a near-max gas estimate should report unhealthy, not overflow into passing")
	}
}
