package store

import (
	"context"
	"encoding/json"
	"time"

	"aetherlay/internal/config"
	"aetherlay/internal/metrics"

	"github.com/rs/zerolog/log"
	"github.com/valkey-io/valkey-go"
)

// CapacityEstimate represents the AIMD-learned safe throughput ceiling for an endpoint,
// derived from observed rate-limit hits rather than an operator-declared CapacityLimit.
// Unlike RateLimitState (a binary "currently blocked" flag with a 24h TTL that must
// self-heal), this is valuable precisely because it persists indefinitely - the AIMD
// decrease step is itself the safety bound against a stale-high estimate.
type CapacityEstimate struct {
	HasEstimate    bool      `json:"has_estimate"`     // Whether any evidence has been observed yet
	MaxRequests    int64     `json:"max_requests"`     // Learned ceiling as of the last decrease/seed
	IncreaseStep   int64     `json:"increase_step"`    // Additive step size, recomputed at each decrease
	WindowSeconds  int       `json:"window_seconds"`   // Frozen at seed time, stable even if config later changes
	LastDecreaseAt time.Time `json:"last_decrease_at"` // Anchors both the decrease cooldown and the increase clock
}

// ApplyCapacityDecrease computes the next CapacityEstimate after a confirmed rate-limit
// hit. effectiveNow is the currently effective ceiling (after any lazy growth since the
// last decrease, see EffectiveMaxRequests) - passing 0 when prior.HasEstimate is false is
// fine, since it's only consulted when there is a prior estimate to compare against.
// observedCount is the real number of requests dispatched to this endpoint in the
// current window at the moment of the hit (from GetCapacityCount), which already
// includes the rejected attempt itself.
//
// The next ceiling is grounded in whichever of the two is more conservative: if growth
// had crept the effective ceiling above what was actually observed this window, trust
// the observed count instead of compounding off a purely speculative grown value. When
// there is no prior estimate, this collapses to using the observed count directly - the
// seed and every subsequent decrease are the same operation.
func ApplyCapacityDecrease(prior CapacityEstimate, effectiveNow int64, observedCount int64, windowSeconds int, params config.CapacityLearning, now time.Time) CapacityEstimate {
	base := observedCount
	if prior.HasEstimate && effectiveNow < base {
		base = effectiveNow
	}

	newMax := int64(float64(base) * params.DecreaseFactor)
	if newMax < int64(params.MinEstimate) {
		newMax = int64(params.MinEstimate)
	}

	step := newMax / 10
	if step < 1 {
		step = 1
	}

	return CapacityEstimate{
		HasEstimate:    true,
		MaxRequests:    newMax,
		IncreaseStep:   step,
		WindowSeconds:  windowSeconds,
		LastDecreaseAt: now,
	}
}

// ShouldApplyCapacityDecrease reports whether enough time has passed since the last
// decrease to apply another one. It uses the window width itself as the cooldown, since
// a ceiling can't meaningfully be learned to be "worse" faster than one window boundary,
// and this collapses multiple near-simultaneous rate-limit hits from a single episode
// (e.g. several in-flight retries all rejected within the same window) into one decrease.
func ShouldApplyCapacityDecrease(estimate CapacityEstimate, windowSeconds int, now time.Time) bool {
	if !estimate.HasEstimate {
		return true
	}
	return now.Sub(estimate.LastDecreaseAt) >= time.Duration(windowSeconds)*time.Second
}

// EffectiveMaxRequests computes the currently believed-safe ceiling, growing it
// additively based purely on elapsed wall-clock time since the last decrease. This is
// deliberately stateless between reads - identical on every replica from the same
// persisted estimate and current time, with no background goroutine and no counter to
// race on.
func EffectiveMaxRequests(estimate CapacityEstimate, params config.CapacityLearning, now time.Time) int64 {
	if !estimate.HasEstimate {
		return 0
	}
	if params.IncreaseInterval <= 0 {
		return estimate.MaxRequests
	}
	elapsed := now.Sub(estimate.LastDecreaseAt)
	if elapsed <= 0 {
		return estimate.MaxRequests
	}
	steps := int64(elapsed / (time.Duration(params.IncreaseInterval) * time.Second))
	return estimate.MaxRequests + steps*estimate.IncreaseStep
}

// GetCapacityEstimate retrieves the learned capacity estimate for an endpoint from
// Valkey. Returns a zero-value estimate (HasEstimate: false) if none has been recorded.
func (r *ValkeyClient) GetCapacityEstimate(ctx context.Context, chain, endpoint string) (*CapacityEstimate, error) {
	key := capacityEstimatePrefix + chain + ":" + endpoint
	cmd := r.client.B().Get().Key(key).Build()
	result := r.client.Do(ctx, cmd)

	if valkey.IsValkeyNil(result.Error()) {
		return &CapacityEstimate{}, nil
	}

	data, err := result.AsBytes()
	if err != nil {
		return nil, err
	}

	var estimate CapacityEstimate
	if err := json.Unmarshal(data, &estimate); err != nil {
		return nil, err
	}
	return &estimate, nil
}

// SetCapacityEstimate stores the learned capacity estimate for an endpoint in Valkey,
// with no expiration - unlike RateLimitState, a learned throughput number is valuable
// because it persists through long clean periods; the AIMD decrease step at the next
// real hit is the safety bound against a stale-high value, not a TTL.
// Uses last-write-wins semantics: a concurrent double-decrease across replicas only
// errs toward a more conservative estimate, which the additive-increase step corrects.
func (r *ValkeyClient) SetCapacityEstimate(ctx context.Context, chain, endpoint string, estimate CapacityEstimate) error {
	key := capacityEstimatePrefix + chain + ":" + endpoint

	jsonBytes, err := json.Marshal(estimate)
	if err != nil {
		return err
	}

	cmd := r.client.B().Set().Key(key).Value(string(jsonBytes)).Build()
	return r.client.Do(ctx, cmd).Error()
}

// ApplyLearnedCapacityDecreaseIfEligible is called on every confirmed rate-limit signal.
// It only ever engages for endpoints with no static Capacity configured - static config,
// if present, is left completely untouched. A cooldown (the learning window itself)
// collapses several near-simultaneous hits from one episode into a single decrease.
//
// This is shared verbatim between the load balancer (internal/server) and the standalone
// health checker (services/health-checker) - both mutate the same Valkey-persisted
// estimate for a given endpoint, so they must apply identical math or silently disagree
// about what the learned ceiling means for that endpoint.
func ApplyLearnedCapacityDecreaseIfEligible(ctx context.Context, valkeyClient ValkeyClientIface, chain, endpointID string, ep config.Endpoint, capacityThrottlingEnabled, capacityLearningEnabled bool) {
	if !capacityThrottlingEnabled || !capacityLearningEnabled || ep.Capacity != nil {
		return
	}

	params := config.ResolveCapacityLearning(ep.CapacityLearning)
	now := time.Now()

	prior, err := valkeyClient.GetCapacityEstimate(ctx, chain, endpointID)
	if err != nil {
		log.Debug().Err(err).Str("chain", chain).Str("endpoint", endpointID).Msg("Failed to get capacity estimate")
		return
	}

	// Once an estimate exists, its own WindowSeconds is permanently authoritative - not
	// just for this read, but for the estimate this decrease persists below. Otherwise a
	// later config change to the learning window would both target a different Valkey
	// bucket key than the dispatch-time usage writes (making observedCount stale/zero)
	// and silently re-freeze the estimate to the new value, contradicting the documented
	// invariant that the window is frozen for the lifetime of the estimate. Only before
	// any estimate has been seeded is there no frozen value yet, so the live-resolved
	// config is used - and that first decrease is what freezes it from then on.
	windowSeconds := params.WindowSeconds
	if prior.HasEstimate {
		windowSeconds = prior.WindowSeconds
	}

	if !ShouldApplyCapacityDecrease(*prior, windowSeconds, now) {
		log.Debug().Str("chain", chain).Str("endpoint", endpointID).Msg("Skipping capacity estimate decrease, within cooldown of the last decrease")
		return
	}

	observedCount, err := valkeyClient.GetCapacityCount(ctx, chain, endpointID, windowSeconds)
	if err != nil {
		log.Debug().Err(err).Str("chain", chain).Str("endpoint", endpointID).Msg("Failed to get capacity count for estimate decrease")
		return
	}

	effectiveNow := EffectiveMaxRequests(*prior, params, now)
	newEstimate := ApplyCapacityDecrease(*prior, effectiveNow, observedCount, windowSeconds, params, now)

	if err := valkeyClient.SetCapacityEstimate(ctx, chain, endpointID, newEstimate); err != nil {
		log.Debug().Err(err).Str("chain", chain).Str("endpoint", endpointID).Msg("Failed to set capacity estimate")
		return
	}

	log.Info().Str("chain", chain).Str("endpoint", endpointID).Int64("new_estimate", newEstimate.MaxRequests).Int("window_seconds", newEstimate.WindowSeconds).Msg("Decreased learned capacity estimate after a rate-limit hit")
	if metrics.EndpointCapacityEstimatedCeiling != nil {
		metrics.EndpointCapacityEstimatedCeiling.WithLabelValues(chain, endpointID).Set(float64(newEstimate.MaxRequests))
	}
	if metrics.EndpointCapacityEstimateDecreasedTotal != nil {
		metrics.EndpointCapacityEstimateDecreasedTotal.WithLabelValues(chain, endpointID).Inc()
	}
}
