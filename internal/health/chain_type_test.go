package health

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"aetherlay/internal/config"
	"aetherlay/internal/store"
)

func TestBlockNumberMethodByChainType(t *testing.T) {
	if got := blockNumberMethod(config.ChainTypeEVM); got != "eth_blockNumber" {
		t.Errorf("expected eth_blockNumber for evm, got %q", got)
	}
	if got := blockNumberMethod(config.ChainTypeSolana); got != "getSlot" {
		t.Errorf("expected getSlot for solana, got %q", got)
	}
	if got := blockNumberMethod(""); got != "eth_blockNumber" {
		t.Errorf("expected eth_blockNumber for empty chain type (evm default), got %q", got)
	}
}

func TestSyncStatusMethodByChainType(t *testing.T) {
	if got := syncStatusMethod(config.ChainTypeEVM); got != "eth_syncing" {
		t.Errorf("expected eth_syncing for evm, got %q", got)
	}
	if got := syncStatusMethod(config.ChainTypeSolana); got != "getHealth" {
		t.Errorf("expected getHealth for solana, got %q", got)
	}
}

func TestParseSolanaSlot(t *testing.T) {
	if n, healthy := ParseSolanaSlot(float64(12345)); !healthy || n != 12345 {
		t.Errorf("expected healthy slot 12345, got %d healthy=%v", n, healthy)
	}
	if _, healthy := ParseSolanaSlot(float64(0)); healthy {
		t.Error("slot 0 should be unhealthy")
	}
	if _, healthy := ParseSolanaSlot("not-a-number"); healthy {
		t.Error("a non-numeric slot result should be unhealthy")
	}
	if _, healthy := ParseSolanaSlot(nil); healthy {
		t.Error("a nil slot result should be unhealthy")
	}
}

func TestParseBlockResultDispatchesByChainType(t *testing.T) {
	if n, healthy := ParseBlockResult(config.ChainTypeEVM, "0x10"); !healthy || n != 16 {
		t.Errorf("expected healthy EVM block 16, got %d healthy=%v", n, healthy)
	}
	if n, healthy := ParseBlockResult(config.ChainTypeSolana, float64(99)); !healthy || n != 99 {
		t.Errorf("expected healthy Solana slot 99, got %d healthy=%v", n, healthy)
	}
	// A Solana-shaped (plain number) result should not parse as a valid EVM block, and
	// vice versa - the two dialects must not silently cross-parse each other's results.
	if _, healthy := ParseBlockResult(config.ChainTypeEVM, float64(99)); healthy {
		t.Error("a plain number should not parse as a healthy EVM block number")
	}
	if _, healthy := ParseBlockResult(config.ChainTypeSolana, "0x10"); healthy {
		t.Error("a hex string should not parse as a healthy Solana slot")
	}
}

func TestParseSyncStatusForChainType(t *testing.T) {
	if !parseSyncStatusForChainType(config.ChainTypeEVM, false) {
		t.Error("eth_syncing=false should be healthy")
	}
	if parseSyncStatusForChainType(config.ChainTypeEVM, true) {
		t.Error("eth_syncing=true should be unhealthy")
	}
	if !parseSyncStatusForChainType(config.ChainTypeSolana, "ok") {
		t.Error("getHealth=\"ok\" should be healthy")
	}
	if parseSyncStatusForChainType(config.ChainTypeSolana, "behind") {
		t.Error("getHealth!=\"ok\" should be unhealthy")
	}
}

// TestCheckRPCErrorTreatsSolanaGetHealthMethodNotFoundAsOptional mirrors the existing
// eth_syncing leniency: an endpoint that doesn't implement getHealth shouldn't be marked
// unhealthy just because the optional sync-status call is unsupported.
func TestCheckRPCErrorTreatsSolanaGetHealthMethodNotFoundAsOptional(t *testing.T) {
	var response RpcResponse
	if err := json.Unmarshal([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"Method not found"}}`), &response); err != nil {
		t.Fatalf("failed to unmarshal fixture: %v", err)
	}

	err := checkRPCError(&response, "getHealth", "HTTP", "solana-mainnet", "test-1", "https://example.com")
	if !errors.Is(err, ErrMethodNotFound) {
		t.Errorf("expected ErrMethodNotFound for missing getHealth, got %v", err)
	}
}

// TestCheckRPCErrorTreatsSolanaGetSlotMethodNotFoundAsFatal is the inverse: getSlot is
// the required block/slot call, so its absence must still be a hard failure, not
// tolerated like the optional getHealth call.
func TestCheckRPCErrorTreatsSolanaGetSlotMethodNotFoundAsFatal(t *testing.T) {
	var response RpcResponse
	if err := json.Unmarshal([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"Method not found"}}`), &response); err != nil {
		t.Fatalf("failed to unmarshal fixture: %v", err)
	}

	err := checkRPCError(&response, "getSlot", "HTTP", "solana-mainnet", "test-1", "https://example.com")
	if err == nil || errors.Is(err, ErrMethodNotFound) {
		t.Errorf("expected a fatal (non-ErrMethodNotFound) error for missing getSlot, got %v", err)
	}
}

// solanaRPCTestServer simulates a minimal Solana JSON-RPC endpoint supporting getSlot
// and getHealth, so checkHTTPHealth/checkWSHealth can be exercised end-to-end.
func solanaRPCTestServer(t *testing.T, slot int64, healthy bool) *httptest.Server {
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
			if healthy {
				json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": "ok"})
			} else {
				json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "error": map[string]any{"code": -32005, "message": "Node is unhealthy"}})
			}
		default:
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "error": map[string]any{"code": -32601, "message": "Method not found"}})
		}
	}))
}

func TestCheckHTTPHealthSolanaEndpointHealthy(t *testing.T) {
	server := solanaRPCTestServer(t, 123456, true)
	defer server.Close()

	checker := &Checker{
		valkeyClient:          store.NewMockValkeyClient(),
		healthCheckSyncStatus: true,
	}
	endpoint := config.Endpoint{
		Provider:  "test",
		ChainType: config.ChainTypeSolana,
		HTTPURL:   server.URL,
	}

	if healthy := checker.checkHTTPHealth(context.Background(), "solana-mainnet", "test-1", endpoint); !healthy {
		t.Error("expected a healthy Solana endpoint (valid slot, getHealth=ok) to report healthy")
	}
}

func TestCheckHTTPHealthSolanaEndpointUnhealthy(t *testing.T) {
	server := solanaRPCTestServer(t, 123456, false)
	defer server.Close()

	checker := &Checker{
		valkeyClient:          store.NewMockValkeyClient(),
		healthCheckSyncStatus: true,
	}
	endpoint := config.Endpoint{
		Provider:  "test",
		ChainType: config.ChainTypeSolana,
		HTTPURL:   server.URL,
	}

	if healthy := checker.checkHTTPHealth(context.Background(), "solana-mainnet", "test-1", endpoint); healthy {
		t.Error("expected an endpoint failing getHealth to report unhealthy")
	}
}

// TestCheckHTTPHealthSolanaEndpointWithoutSyncCheck is a regression guard against
// the EVM code path leaking back in: hitting a Solana endpoint with eth_blockNumber
// instead of getSlot would always fail, since the fixture server only understands
// getSlot/getHealth.
func TestCheckHTTPHealthSolanaEndpointWithoutSyncCheck(t *testing.T) {
	server := solanaRPCTestServer(t, 123456, true)
	defer server.Close()

	checker := &Checker{
		valkeyClient:          store.NewMockValkeyClient(),
		healthCheckSyncStatus: false,
	}
	endpoint := config.Endpoint{
		Provider:  "test",
		ChainType: config.ChainTypeSolana,
		HTTPURL:   server.URL,
	}

	if healthy := checker.checkHTTPHealth(context.Background(), "solana-mainnet", "test-1", endpoint); !healthy {
		t.Error("expected a healthy Solana endpoint to report healthy even with sync-status checking disabled")
	}
}

// TestCheckHTTPHealthEVMEndpointUnaffectedByChainType is a regression guard: an endpoint
// with ChainType left at the "evm" default must keep using eth_blockNumber/eth_syncing.
func TestCheckHTTPHealthEVMEndpointUnaffectedByChainType(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		method, _ := req["method"].(string)

		w.Header().Set("Content-Type", "application/json")
		switch method {
		case "eth_blockNumber":
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": "0x10"})
		case "eth_syncing":
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": false})
		default:
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "error": map[string]any{"code": -32601, "message": "Method not found"}})
		}
	}))
	defer server.Close()

	checker := &Checker{
		valkeyClient:          store.NewMockValkeyClient(),
		healthCheckSyncStatus: true,
	}
	endpoint := config.Endpoint{
		Provider:  "test",
		ChainType: config.ChainTypeEVM,
		HTTPURL:   server.URL,
	}

	if healthy := checker.checkHTTPHealth(context.Background(), "eth-mainnet", "test-1", endpoint); !healthy {
		t.Error("expected a healthy EVM endpoint to still report healthy")
	}
}
