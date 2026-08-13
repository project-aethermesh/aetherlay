package health

// solanaFinalizedSlotMargin keeps the canned getBlock probe well behind the reported tip
// slot so it targets a rooted, finalized block instead of one that may not exist yet.
const solanaFinalizedSlotMargin = 32

// customProbeBuilders maps a JSON-RPC method observed failing on real proxied traffic to
// a canned, read-only request Aetherlay can safely issue on its own to specifically
// re-test that method. This is deliberately not the client's original request body;
// replaying an arbitrary captured request could resubmit a state-mutating call such as
// sendTransaction. Only methods with a well-defined, side-effect-free, always-valid
// request shape belong here. currentBlockOrSlot is the value the calling check just
// obtained from the endpoint's regular getSlot/eth_blockNumber probe.
var customProbeBuilders = map[string]func(currentBlockOrSlot int64) (method string, params []any){
	// Solana
	"getBlock": func(slot int64) (string, []any) {
		target := max(slot-solanaFinalizedSlotMargin, 0)
		return "getBlock", []any{target, map[string]any{
			"encoding":                       "json",
			"maxSupportedTransactionVersion": 0,
		}}
	},

	// EVM
	"eth_getBlockByNumber": func(_ int64) (string, []any) {
		return "eth_getBlockByNumber", []any{"latest", false}
	},
}

// IsCustomProbeMethod reports whether method is on the allowlist of methods Aetherlay
// knows how to safely re-test on its own via customProbeBuilders.
func IsCustomProbeMethod(method string) bool {
	_, ok := customProbeBuilders[method]
	return ok
}
