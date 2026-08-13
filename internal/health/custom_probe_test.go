package health

import (
	"testing"

	"aetherlay/internal/config"
)

func TestCustomProbeBuilderGetBlockUsesMarginBehindTip(t *testing.T) {
	build, ok := customProbeBuilderFor("getBlock", config.ChainTypeSolana)
	if !ok {
		t.Fatal("expected getBlock to be a registered custom probe builder for Solana")
	}

	method, params := build(1000)
	if method != "getBlock" {
		t.Errorf("expected method getBlock, got %q", method)
	}
	if len(params) != 2 {
		t.Fatalf("expected 2 params, got %d: %v", len(params), params)
	}
	slot, ok := params[0].(int64)
	if !ok || slot != 1000-solanaFinalizedSlotMargin {
		t.Errorf("expected slot %d, got %v", 1000-solanaFinalizedSlotMargin, params[0])
	}
}

func TestCustomProbeBuilderGetBlockClampsToZero(t *testing.T) {
	build, _ := customProbeBuilderFor("getBlock", config.ChainTypeSolana)

	_, params := build(5) // well under solanaFinalizedSlotMargin
	slot, ok := params[0].(int64)
	if !ok || slot != 0 {
		t.Errorf("expected slot to clamp to 0 for a low current slot, got %v", params[0])
	}
}

func TestCustomProbeBuilderEthGetBlockByNumberIsAlwaysLatest(t *testing.T) {
	build, ok := customProbeBuilderFor("eth_getBlockByNumber", config.ChainTypeEVM)
	if !ok {
		t.Fatal("expected eth_getBlockByNumber to be a registered custom probe builder for EVM")
	}

	method, params := build(999999)
	if method != "eth_getBlockByNumber" {
		t.Errorf("expected method eth_getBlockByNumber, got %q", method)
	}
	if len(params) != 2 || params[0] != "latest" || params[1] != false {
		t.Errorf("expected params [\"latest\", false], got %v", params)
	}
}

// TestCustomProbeBuilderForRejectsMismatchedChainType is a regression guard: getBlock is a
// real Solana method name, but must never be treated as valid for an EVM endpoint (or the
// reverse for eth_getBlockByNumber), even though both are registered method names.
func TestCustomProbeBuilderForRejectsMismatchedChainType(t *testing.T) {
	if _, ok := customProbeBuilderFor("getBlock", config.ChainTypeEVM); ok {
		t.Error("expected getBlock to be rejected for an EVM endpoint")
	}
	if _, ok := customProbeBuilderFor("eth_getBlockByNumber", config.ChainTypeSolana); ok {
		t.Error("expected eth_getBlockByNumber to be rejected for a Solana endpoint")
	}
	// An empty ChainType defaults to EVM elsewhere in this package (blockNumberMethod,
	// syncStatusMethod); the same default must apply here too.
	if _, ok := customProbeBuilderFor("getBlock", ""); ok {
		t.Error("expected getBlock to be rejected for an endpoint with no configured chain type (defaults to EVM)")
	}
	if _, ok := customProbeBuilderFor("eth_getBlockByNumber", ""); !ok {
		t.Error("expected eth_getBlockByNumber to be accepted for an endpoint with no configured chain type (defaults to EVM)")
	}
}

func TestIsCustomProbeMethod(t *testing.T) {
	if !IsCustomProbeMethod("getBlock", config.ChainTypeSolana) {
		t.Error("expected getBlock to be allowlisted for Solana")
	}
	if !IsCustomProbeMethod("eth_getBlockByNumber", config.ChainTypeEVM) {
		t.Error("expected eth_getBlockByNumber to be allowlisted for EVM")
	}
	if IsCustomProbeMethod("getBlock", config.ChainTypeEVM) {
		t.Error("expected getBlock to not be allowlisted for EVM")
	}
	if IsCustomProbeMethod("eth_getBlockByNumber", config.ChainTypeSolana) {
		t.Error("expected eth_getBlockByNumber to not be allowlisted for Solana")
	}
	if IsCustomProbeMethod("sendTransaction", config.ChainTypeSolana) {
		t.Error("expected sendTransaction to not be allowlisted")
	}
	if IsCustomProbeMethod("", config.ChainTypeSolana) {
		t.Error("expected an empty method to not be allowlisted")
	}
}
