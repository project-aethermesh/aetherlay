package health

import "aetherlay/internal/config"

// solanaFinalizedSlotMargin keeps the canned getBlock probe well behind the reported tip
// slot so it targets a rooted, finalized block instead of one that may not exist yet.
const solanaFinalizedSlotMargin = 32

// customProbeBuilder pairs a canned request builder with the chain type it's valid for.
// getBlock and eth_getBlockByNumber are both real method names, but only for their own
// dialect; a Solana endpoint erroring on an EVM-shaped request name (or the reverse)
// must never be captured or replayed as that method, since the probe would just fail
// against the wrong chain type.
type customProbeBuilder struct {
	chainType string
	build     func(currentBlockOrSlot int64) (method string, params []any)
}

// customProbeBuilders maps a JSON-RPC method observed failing on real proxied traffic to
// a canned, read-only request Aetherlay can safely issue on its own to specifically
// re-test that method. This is deliberately not the client's original request body;
// replaying an arbitrary captured request could resubmit a state-mutating call such as
// sendTransaction. Only methods with a well-defined, side-effect-free, always-valid
// request shape belong here. currentBlockOrSlot is the value the calling check just
// obtained from the endpoint's regular getSlot/eth_blockNumber probe.
var customProbeBuilders = map[string]customProbeBuilder{
	"getBlock": {
		chainType: config.ChainTypeSolana,
		build: func(slot int64) (string, []any) {
			target := max(slot-solanaFinalizedSlotMargin, 0)
			return "getBlock", []any{target, map[string]any{
				"encoding":                       "json",
				"maxSupportedTransactionVersion": 0,
			}}
		},
	},
	"eth_getBlockByNumber": {
		chainType: config.ChainTypeEVM,
		build: func(_ int64) (string, []any) {
			return "eth_getBlockByNumber", []any{"latest", false}
		},
	},
}

// normalizeChainType mirrors the default-to-EVM behavior blockNumberMethod/syncStatusMethod
// already apply elsewhere in this package: an endpoint with ChainType left unset is EVM.
func normalizeChainType(chainType string) string {
	if chainType == config.ChainTypeSolana {
		return config.ChainTypeSolana
	}
	return config.ChainTypeEVM
}

// IsCustomProbeMethod reports whether method is on the allowlist of methods Aetherlay
// knows how to safely re-test on its own via customProbeBuilders, for the given endpoint
// chain type. A method valid for one chain type is never treated as valid for another.
func IsCustomProbeMethod(method, chainType string) bool {
	entry, ok := customProbeBuilders[method]
	return ok && entry.chainType == normalizeChainType(chainType)
}

// customProbeBuilderFor returns the canned request builder for method, scoped to
// chainType, or false if method isn't allowlisted for that chain type.
func customProbeBuilderFor(method, chainType string) (func(currentBlockOrSlot int64) (string, []any), bool) {
	entry, ok := customProbeBuilders[method]
	if !ok || entry.chainType != normalizeChainType(chainType) {
		return nil, false
	}
	return entry.build, true
}
