package health

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"aetherlay/internal/config"
)

// RateLimitSignal describes a detected rate-limit condition and any recovery timing
// hint the provider gave, so callers can seed a precise backoff instead of guessing.
type RateLimitSignal struct {
	IsRateLimited bool
	IsDailyQuota  bool          // true only for Infura's HTTP 402 daily credit cap - can't be sped up by probing
	RetryAfter    time.Duration // 0 if absent/unparseable
}

// IsJSONRPCRateLimitCode reports whether a JSON-RPC error code indicates rate limiting.
// -32005 is the standard "Request limit exceeded" code used by Infura, Alchemy, and others.
func IsJSONRPCRateLimitCode(code int) bool {
	return code == -32005
}

// DetectRateLimit inspects an HTTP response (status code + headers) and, when available,
// a parsed JSON-RPC response body, to determine whether a request was rate limited and
// what recovery timing hint (if any) the provider gave.
//
// This has exactly one provider-specific branch (Infura's HTTP 402 daily-credit-cap
// convention). Alchemy's unreliable Retry-After and dRPC's total absence of rate-limit
// headers are both handled by the same generic path - a plugin/adapter system isn't
// warranted for a single behavioral axis across the providers this was built against.
func DetectRateLimit(provider string, statusCode int, headers http.Header, rpcResp *RpcResponse) RateLimitSignal {
	var sig RateLimitSignal

	switch {
	case statusCode == http.StatusTooManyRequests:
		sig.IsRateLimited = true
	case statusCode == http.StatusPaymentRequired && strings.EqualFold(provider, "infura"):
		// Infura-documented behavior: 402 means the daily credit cap is exhausted for
		// the rest of the day, not a short burst limit - kept distinct so recovery
		// doesn't re-probe on the same short interval as a 429 burst limit.
		sig.IsRateLimited = true
		sig.IsDailyQuota = true
	case rpcResp != nil && rpcResp.Error != nil && IsJSONRPCRateLimitCode(rpcResp.Error.Code):
		sig.IsRateLimited = true
	}

	if sig.IsRateLimited && headers != nil {
		if ra := headers.Get("Retry-After"); ra != "" {
			if secs, err := strconv.Atoi(strings.TrimSpace(ra)); err == nil && secs > 0 {
				sig.RetryAfter = time.Duration(secs) * time.Second
			} else if t, err := http.ParseTime(ra); err == nil {
				if d := time.Until(t); d > 0 {
					sig.RetryAfter = d
				}
			}
		}
	}

	return sig
}

// InitialBackoffForSignal computes the first recovery-check wait time from whatever the
// provider actually told us, instead of always guessing from the configured
// RateLimitRecovery.InitialBackoff:
//   - A parsed Retry-After is the most precise signal available and is used directly.
//   - Infura's daily credit cap (402) can't be sped up by probing sooner, since it only
//     resets once the day rolls over - Infura's docs don't guarantee an exact reset
//     boundary, so rather than assume one, this seeds at the endpoint's own configured
//     (or default) MaxBackoff, so a known-exhausted daily quota isn't re-probed on a
//     short cycle.
//   - Otherwise 0, which leaves the caller's recovery scheduler to fall back to
//     InitialBackoff, exactly as it did before this signal existed.
//
// This is shared verbatim between the load balancer (internal/server) and the standalone
// health checker (services/health-checker) - both seed the same recovery backoff from the
// same signal, so they must apply identical logic.
func InitialBackoffForSignal(cfg *config.Config, chain, endpointID string, signal RateLimitSignal) int {
	if signal.RetryAfter > 0 {
		return int(signal.RetryAfter.Seconds())
	}
	if signal.IsDailyQuota {
		rlc := config.DefaultRateLimitRecovery()
		if chainEndpoints, ok := cfg.GetEndpointsForChain(chain); ok {
			if ep, ok := chainEndpoints[endpointID]; ok && ep.RateLimitRecovery != nil && ep.RateLimitRecovery.MaxBackoff != 0 {
				rlc.MaxBackoff = ep.RateLimitRecovery.MaxBackoff
			}
		}
		return rlc.MaxBackoff
	}
	return 0
}
