package health

import "testing"

func TestCustomProbeBuilderGetBlockUsesMarginBehindTip(t *testing.T) {
	builder, ok := customProbeBuilders["getBlock"]
	if !ok {
		t.Fatal("expected getBlock to be a registered custom probe builder")
	}

	method, params := builder(1000)
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
	builder := customProbeBuilders["getBlock"]

	_, params := builder(5) // well under solanaFinalizedSlotMargin
	slot, ok := params[0].(int64)
	if !ok || slot != 0 {
		t.Errorf("expected slot to clamp to 0 for a low current slot, got %v", params[0])
	}
}

func TestCustomProbeBuilderEthGetBlockByNumberIsAlwaysLatest(t *testing.T) {
	builder, ok := customProbeBuilders["eth_getBlockByNumber"]
	if !ok {
		t.Fatal("expected eth_getBlockByNumber to be a registered custom probe builder")
	}

	method, params := builder(999999)
	if method != "eth_getBlockByNumber" {
		t.Errorf("expected method eth_getBlockByNumber, got %q", method)
	}
	if len(params) != 2 || params[0] != "latest" || params[1] != false {
		t.Errorf("expected params [\"latest\", false], got %v", params)
	}
}

func TestIsCustomProbeMethod(t *testing.T) {
	if !IsCustomProbeMethod("getBlock") {
		t.Error("expected getBlock to be allowlisted")
	}
	if !IsCustomProbeMethod("eth_getBlockByNumber") {
		t.Error("expected eth_getBlockByNumber to be allowlisted")
	}
	if IsCustomProbeMethod("sendTransaction") {
		t.Error("expected sendTransaction to not be allowlisted")
	}
	if IsCustomProbeMethod("") {
		t.Error("expected an empty method to not be allowlisted")
	}
}
