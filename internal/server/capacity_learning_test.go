package server

import (
	"context"
	"testing"
	"time"

	"aetherlay/internal/config"
	"aetherlay/internal/helpers"
	"aetherlay/internal/store"
)

// learningTestConfig returns a LoadedConfig with capacity throttling and adaptive
// learning both explicitly enabled, since createTestConfig()'s struct literal leaves
// both at their Go zero value (false) - matching how existing, pre-adaptive-learning
// tests continue to see no behavior change unless a test opts in explicitly.
func learningTestConfig() *helpers.LoadedConfig {
	cfg := createTestConfig()
	cfg.CapacityThrottlingEnabled = true
	cfg.CapacityLearningEnabled = true
	return cfg
}

func TestApplyLearnedCapacityDecreaseSeedsFromObservedCount(t *testing.T) {
	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"ethereum": {
				"ep1": config.Endpoint{Provider: "alchemy", Role: "primary", Type: "full", HTTPURL: "http://ep1"},
			},
		},
	}
	valkeyClient := store.NewMockValkeyClient()
	ctx := context.Background()

	// Simulate 10 requests already dispatched in the current (default 60s) learning window.
	for i := 0; i < 10; i++ {
		if _, err := valkeyClient.IncrementCapacityCount(ctx, "ethereum", "ep1", 60); err != nil {
			t.Fatalf("IncrementCapacityCount failed: %v", err)
		}
	}

	server := NewServer(cfg, valkeyClient, learningTestConfig())
	server.applyLearnedCapacityDecrease("ethereum", "ep1", cfg.Endpoints["ethereum"]["ep1"])

	estimate, err := valkeyClient.GetCapacityEstimate(ctx, "ethereum", "ep1")
	if err != nil {
		t.Fatalf("GetCapacityEstimate failed: %v", err)
	}
	if !estimate.HasEstimate {
		t.Fatal("Expected an estimate to be seeded")
	}
	if estimate.MaxRequests != 5 {
		t.Errorf("Expected MaxRequests to be 5 (10 observed * 0.5), got %d", estimate.MaxRequests)
	}
	if estimate.WindowSeconds != 60 {
		t.Errorf("Expected WindowSeconds to be 60 (default learning window), got %d", estimate.WindowSeconds)
	}
}

func TestApplyLearnedCapacityDecreaseSkipsWithinCooldown(t *testing.T) {
	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"ethereum": {
				"ep1": config.Endpoint{Provider: "alchemy", Role: "primary", Type: "full", HTTPURL: "http://ep1"},
			},
		},
	}
	valkeyClient := store.NewMockValkeyClient()
	ctx := context.Background()

	for i := 0; i < 10; i++ {
		valkeyClient.IncrementCapacityCount(ctx, "ethereum", "ep1", 60)
	}

	server := NewServer(cfg, valkeyClient, learningTestConfig())
	ep := cfg.Endpoints["ethereum"]["ep1"]

	server.applyLearnedCapacityDecrease("ethereum", "ep1", ep)
	first, _ := valkeyClient.GetCapacityEstimate(ctx, "ethereum", "ep1")
	if first.MaxRequests != 5 {
		t.Fatalf("Expected first decrease to seed 5, got %d", first.MaxRequests)
	}

	// Second hit immediately after - within the cooldown (the learning window itself) -
	// must not decrease again even though the observed count hasn't changed.
	server.applyLearnedCapacityDecrease("ethereum", "ep1", ep)
	second, _ := valkeyClient.GetCapacityEstimate(ctx, "ethereum", "ep1")
	if second.MaxRequests != 5 {
		t.Errorf("Expected MaxRequests to remain 5 (no double decrease within cooldown), got %d", second.MaxRequests)
	}
}

func TestApplyLearnedCapacityDecreaseAppliesAgainAfterCooldownElapsed(t *testing.T) {
	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"ethereum": {
				"ep1": config.Endpoint{Provider: "alchemy", Role: "primary", Type: "full", HTTPURL: "http://ep1"},
			},
		},
	}
	valkeyClient := store.NewMockValkeyClient()
	ctx := context.Background()
	ep := cfg.Endpoints["ethereum"]["ep1"]

	for i := 0; i < 10; i++ {
		valkeyClient.IncrementCapacityCount(ctx, "ethereum", "ep1", 60)
	}

	server := NewServer(cfg, valkeyClient, learningTestConfig())
	server.applyLearnedCapacityDecrease("ethereum", "ep1", ep)

	seeded, _ := valkeyClient.GetCapacityEstimate(ctx, "ethereum", "ep1")
	if seeded.MaxRequests != 5 {
		t.Fatalf("Expected seed to be 5, got %d", seeded.MaxRequests)
	}

	// Backdate LastDecreaseAt past both the cooldown and two IncreaseIntervals (60s each,
	// default), so growth applies before the next decrease grounds itself in whichever of
	// the grown estimate or fresh observed count is lower.
	seeded.LastDecreaseAt = time.Now().Add(-125 * time.Second)
	if err := valkeyClient.SetCapacityEstimate(ctx, "ethereum", "ep1", *seeded); err != nil {
		t.Fatalf("SetCapacityEstimate failed: %v", err)
	}

	// More traffic since the (backdated) last decrease: 6 additional requests, 16 total
	// in the window bucket.
	for i := 0; i < 6; i++ {
		valkeyClient.IncrementCapacityCount(ctx, "ethereum", "ep1", 60)
	}

	server.applyLearnedCapacityDecrease("ethereum", "ep1", ep)

	final, err := valkeyClient.GetCapacityEstimate(ctx, "ethereum", "ep1")
	if err != nil {
		t.Fatalf("GetCapacityEstimate failed: %v", err)
	}
	// effectiveNow = 5 (seed) + 2 steps * 1 (step, max(1, 5/10)) = 7
	// observedCount = 16
	// base = min(7, 16) = 7 -> newMax = floor(7*0.5) = 3
	if final.MaxRequests != 3 {
		t.Errorf("Expected second decrease to be grounded in the grown estimate (7) not raw observed count (16): expected 3, got %d", final.MaxRequests)
	}
}

func TestApplyLearnedCapacityDecreaseSkipsWhenStaticCapacityConfigured(t *testing.T) {
	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"ethereum": {
				"ep1": config.Endpoint{
					Provider: "alchemy", Role: "primary", Type: "full", HTTPURL: "http://ep1",
					Capacity: &config.CapacityLimit{MaxRequests: 100, WindowSeconds: 10},
				},
			},
		},
	}
	valkeyClient := store.NewMockValkeyClient()
	ctx := context.Background()

	server := NewServer(cfg, valkeyClient, learningTestConfig())
	server.applyLearnedCapacityDecrease("ethereum", "ep1", cfg.Endpoints["ethereum"]["ep1"])

	estimate, _ := valkeyClient.GetCapacityEstimate(ctx, "ethereum", "ep1")
	if estimate.HasEstimate {
		t.Error("Expected no learned estimate when a static Capacity is configured")
	}
}

func TestApplyLearnedCapacityDecreaseSkipsWhenLearningDisabled(t *testing.T) {
	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"ethereum": {
				"ep1": config.Endpoint{Provider: "alchemy", Role: "primary", Type: "full", HTTPURL: "http://ep1"},
			},
		},
	}
	valkeyClient := store.NewMockValkeyClient()
	ctx := context.Background()

	appConfig := learningTestConfig()
	appConfig.CapacityLearningEnabled = false
	server := NewServer(cfg, valkeyClient, appConfig)
	server.applyLearnedCapacityDecrease("ethereum", "ep1", cfg.Endpoints["ethereum"]["ep1"])

	estimate, _ := valkeyClient.GetCapacityEstimate(ctx, "ethereum", "ep1")
	if estimate.HasEstimate {
		t.Error("Expected no learned estimate when CapacityLearningEnabled is false")
	}
}

func TestEffectiveCapacityCeilingNoBlackHoleBeforeEvidence(t *testing.T) {
	cfg := &config.Config{}
	valkeyClient := store.NewMockValkeyClient()
	server := NewServer(cfg, valkeyClient, learningTestConfig())

	ep := config.Endpoint{Provider: "alchemy"}
	_, _, ok := server.effectiveCapacityCeiling("ethereum", "fresh-endpoint", ep)
	if ok {
		t.Error("Expected no ceiling for an endpoint with no static Capacity and no learned estimate yet")
	}
}

func TestGetAvailableEndpointsSkipsEndpointOnceLearnedEstimateExhausted(t *testing.T) {
	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"ethereum": {
				"learned-tight": config.Endpoint{Provider: "alchemy", Role: "primary", Type: "full", HTTPURL: "http://tight"},
				"unbounded":     config.Endpoint{Provider: "alchemy", Role: "primary", Type: "full", HTTPURL: "http://unbounded"},
			},
		},
	}
	valkeyClient := store.NewMockValkeyClient()
	valkeyClient.PopulateStatuses(map[string]*store.EndpointStatus{
		"ethereum:learned-tight": {HasHTTP: true, HealthyHTTP: true},
		"ethereum:unbounded":     {HasHTTP: true, HealthyHTTP: true},
	})
	ctx := context.Background()

	// Seed a learned estimate for "learned-tight" and drive its window count up to it.
	if err := valkeyClient.SetCapacityEstimate(ctx, "ethereum", "learned-tight", store.CapacityEstimate{
		HasEstimate: true, MaxRequests: 2, IncreaseStep: 1, WindowSeconds: 60, LastDecreaseAt: time.Now(),
	}); err != nil {
		t.Fatalf("SetCapacityEstimate failed: %v", err)
	}
	valkeyClient.IncrementCapacityCount(ctx, "ethereum", "learned-tight", 60)
	valkeyClient.IncrementCapacityCount(ctx, "ethereum", "learned-tight", 60)

	server := NewServer(cfg, valkeyClient, learningTestConfig())
	endpoints := server.getAvailableEndpoints("ethereum", false, false)

	if len(endpoints) != 1 {
		t.Fatalf("Expected 1 available endpoint, got %d", len(endpoints))
	}
	if endpoints[0].ID != "unbounded" {
		t.Errorf("Expected 'unbounded' (no black hole before evidence), got %s", endpoints[0].ID)
	}
}

func TestSelectBestEndpointByRoleWeightsLearnedAgainstStatic(t *testing.T) {
	valkeyClient := store.NewMockValkeyClient()
	ctx := context.Background()

	for i := 0; i < 10; i++ {
		valkeyClient.IncrementRequestCount(ctx, "ethereum", "static-large", "proxy_requests")
		valkeyClient.IncrementRequestCount(ctx, "ethereum", "learned-small", "proxy_requests")
	}

	if err := valkeyClient.SetCapacityEstimate(ctx, "ethereum", "learned-small", store.CapacityEstimate{
		HasEstimate: true, MaxRequests: 10, IncreaseStep: 1, WindowSeconds: 60, LastDecreaseAt: time.Now(),
	}); err != nil {
		t.Fatalf("SetCapacityEstimate failed: %v", err)
	}

	endpoints := []EndpointWithID{
		{ID: "static-large", Endpoint: config.Endpoint{
			Role: "primary", Type: "full", HTTPURL: "http://large",
			Capacity: &config.CapacityLimit{MaxRequests: 1000, WindowSeconds: 1},
		}},
		{ID: "learned-small", Endpoint: config.Endpoint{
			Role: "primary", Type: "full", HTTPURL: "http://small",
		}},
	}

	server := NewServer(&config.Config{}, valkeyClient, learningTestConfig())
	best := server.selectBestEndpointByRole("ethereum", endpoints, "primary")

	if best == nil {
		t.Fatal("Expected a best endpoint")
	}
	// Equal raw counts, but static-large's huge daily-equivalent budget (1000 req/s)
	// gives it a far lower utilization ratio than learned-small's modest learned ceiling.
	if best.ID != "static-large" {
		t.Errorf("Expected static-large (lower utilization ratio), got %s", best.ID)
	}
}

func TestEffectiveCapacityCeilingReflectsLazyGrowth(t *testing.T) {
	cfg := &config.Config{}
	valkeyClient := store.NewMockValkeyClient()
	ctx := context.Background()

	if err := valkeyClient.SetCapacityEstimate(ctx, "ethereum", "ep1", store.CapacityEstimate{
		HasEstimate: true, MaxRequests: 5, IncreaseStep: 1, WindowSeconds: 60,
		LastDecreaseAt: time.Now().Add(-185 * time.Second), // 3 intervals of 60s elapsed
	}); err != nil {
		t.Fatalf("SetCapacityEstimate failed: %v", err)
	}

	server := NewServer(cfg, valkeyClient, learningTestConfig())
	maxRequests, windowSeconds, ok := server.effectiveCapacityCeiling("ethereum", "ep1", config.Endpoint{Provider: "alchemy"})

	if !ok {
		t.Fatal("Expected a resolvable ceiling")
	}
	if maxRequests != 8 { // 5 + floor(185/60)=3 steps * 1
		t.Errorf("Expected grown ceiling of 8, got %d", maxRequests)
	}
	if windowSeconds != 60 {
		t.Errorf("Expected WindowSeconds to be 60, got %d", windowSeconds)
	}
}

// Regression guard: existing static-capacity-only tests in capacity_test.go must see no
// behavior change now that CapacityLearningEnabled exists and defaults to true at
// runtime - createTestConfig() leaves it at Go's zero value (false) unless a test opts
// in, so effectiveCapacityCeiling never consults a learned estimate for them.
func TestEffectiveCapacityCeilingUnaffectedWhenLearningNotOptedIn(t *testing.T) {
	cfg := &config.Config{}
	valkeyClient := store.NewMockValkeyClient()
	appConfig := createTestConfig()
	appConfig.CapacityThrottlingEnabled = true
	// appConfig.CapacityLearningEnabled left at zero-value false, as in every pre-existing test.
	server := NewServer(cfg, valkeyClient, appConfig)

	_, _, ok := server.effectiveCapacityCeiling("ethereum", "ep1", config.Endpoint{Provider: "alchemy"})
	if ok {
		t.Error("Expected no ceiling for an unconfigured endpoint when learning is not opted in")
	}
}

// TestCapacityWindowSecondsUsesFrozenEstimateWindow guards against a regression where
// the write path (recordCapacityUsage) resolved window_seconds from the live config while
// the read/gating path (effectiveCapacityCeiling) used the estimate's frozen window -
// causing writes and reads to target different Valkey bucket keys and silently
// defeating the proactive gate. Once an estimate is seeded, both paths must agree.
func TestCapacityWindowSecondsUsesFrozenEstimateWindow(t *testing.T) {
	cfg := &config.Config{}
	valkeyClient := store.NewMockValkeyClient()
	server := NewServer(cfg, valkeyClient, learningTestConfig())

	// Endpoint has no static Capacity; its live-resolved learning window (30s) differs
	// from the frozen window already recorded on its estimate (60s) - simulating an
	// operator changing capacity_learning.window_seconds after the estimate was seeded.
	ep := config.Endpoint{
		Provider:         "alchemy",
		CapacityLearning: &config.CapacityLearning{WindowSeconds: 30},
	}
	if err := valkeyClient.SetCapacityEstimate(context.Background(), "ethereum", "ep1", store.CapacityEstimate{
		HasEstimate: true, MaxRequests: 10, IncreaseStep: 1, WindowSeconds: 60, LastDecreaseAt: time.Now(),
	}); err != nil {
		t.Fatalf("SetCapacityEstimate failed: %v", err)
	}

	if got := server.capacityWindowSeconds("ethereum", "ep1", ep); got != 60 {
		t.Errorf("Expected capacityWindowSeconds to return the frozen estimate window (60), got %d", got)
	}

	// Bootstrap case: no estimate exists yet for this endpoint - falls back to the live
	// config, since there's no frozen value yet to match.
	if got := server.capacityWindowSeconds("ethereum", "no-estimate-yet", ep); got != 30 {
		t.Errorf("Expected capacityWindowSeconds to fall back to the live config (30) before any estimate exists, got %d", got)
	}
}

// TestRecordCapacityUsageWritesToFrozenWindowBucket confirms recordCapacityUsage's write
// lands in the same Valkey bucket effectiveCapacityCeiling's gating check reads from,
// even when the endpoint's live-resolved learning window has since changed.
func TestRecordCapacityUsageWritesToFrozenWindowBucket(t *testing.T) {
	cfg := &config.Config{}
	valkeyClient := store.NewMockValkeyClient()
	appConfig := learningTestConfig()
	server := NewServer(cfg, valkeyClient, appConfig)
	ctx := context.Background()

	ep := config.Endpoint{
		Provider:         "alchemy",
		CapacityLearning: &config.CapacityLearning{WindowSeconds: 30}, // live value, deliberately different from frozen
	}
	if err := valkeyClient.SetCapacityEstimate(ctx, "ethereum", "ep1", store.CapacityEstimate{
		HasEstimate: true, MaxRequests: 10, IncreaseStep: 1, WindowSeconds: 60, LastDecreaseAt: time.Now(),
	}); err != nil {
		t.Fatalf("SetCapacityEstimate failed: %v", err)
	}

	server.recordCapacityUsage("ethereum", ep, "ep1")

	frozenWindowCount, err := valkeyClient.GetCapacityCount(ctx, "ethereum", "ep1", 60)
	if err != nil {
		t.Fatalf("GetCapacityCount failed: %v", err)
	}
	if frozenWindowCount != 1 {
		t.Errorf("Expected the write to land in the frozen-window (60s) bucket, got count %d there", frozenWindowCount)
	}

	liveWindowCount, err := valkeyClient.GetCapacityCount(ctx, "ethereum", "ep1", 30)
	if err != nil {
		t.Fatalf("GetCapacityCount failed: %v", err)
	}
	if liveWindowCount != 0 {
		t.Errorf("Expected nothing written to the stale live-window (30s) bucket, got count %d there", liveWindowCount)
	}

	// The gating path must observe the same count recordCapacityUsage just wrote.
	maxRequests, windowSeconds, ok := server.effectiveCapacityCeiling("ethereum", "ep1", ep)
	if !ok {
		t.Fatal("Expected a resolvable ceiling")
	}
	gatedCount, err := valkeyClient.GetCapacityCount(ctx, "ethereum", "ep1", windowSeconds)
	if err != nil {
		t.Fatalf("GetCapacityCount failed: %v", err)
	}
	if gatedCount != 1 {
		t.Errorf("Expected the gating path to observe the write (count=1) via its own resolved window (%d), got count %d", windowSeconds, gatedCount)
	}
	_ = maxRequests
}

// TestApplyLearnedCapacityDecreaseReadsFrozenWindowNotLiveConfig confirms the decrease
// path's cooldown check and observed-count read are grounded in the estimate's frozen
// window, not a live config value that may have since diverged - otherwise the decrease
// would be seeded from an empty/wrong bucket's count.
func TestApplyLearnedCapacityDecreaseReadsFrozenWindowNotLiveConfig(t *testing.T) {
	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"ethereum": {
				"ep1": config.Endpoint{
					Provider:         "alchemy",
					Role:             "primary",
					Type:             "full",
					HTTPURL:          "http://ep1",
					CapacityLearning: &config.CapacityLearning{WindowSeconds: 30}, // live value, differs from frozen
				},
			},
		},
	}
	valkeyClient := store.NewMockValkeyClient()
	ctx := context.Background()
	ep := cfg.Endpoints["ethereum"]["ep1"]

	// Seed an estimate frozen at 60s, with its cooldown already elapsed.
	if err := valkeyClient.SetCapacityEstimate(ctx, "ethereum", "ep1", store.CapacityEstimate{
		HasEstimate: true, MaxRequests: 10, IncreaseStep: 1, WindowSeconds: 60,
		LastDecreaseAt: time.Now().Add(-120 * time.Second),
	}); err != nil {
		t.Fatalf("SetCapacityEstimate failed: %v", err)
	}

	// Real traffic landed in the frozen (60s) bucket, as recordCapacityUsage would write it.
	for i := 0; i < 8; i++ {
		valkeyClient.IncrementCapacityCount(ctx, "ethereum", "ep1", 60)
	}

	server := NewServer(cfg, valkeyClient, learningTestConfig())
	server.applyLearnedCapacityDecrease("ethereum", "ep1", ep)

	final, err := valkeyClient.GetCapacityEstimate(ctx, "ethereum", "ep1")
	if err != nil {
		t.Fatalf("GetCapacityEstimate failed: %v", err)
	}
	// If the decrease had incorrectly read the live 30s bucket (count=0 there), it would
	// seed from 0 and clamp to MinEstimate (1). Reading the correct frozen 60s bucket
	// (count=8) grounds the decrease at floor(8*0.5)=4 instead.
	if final.MaxRequests != 4 {
		t.Errorf("Expected decrease to be grounded in the frozen window's observed count (8 -> 4), got MaxRequests=%d", final.MaxRequests)
	}
}

func TestApplyLearnedCapacityDecreasePersistsFrozenWindowNotLiveConfig(t *testing.T) {
	cfg := &config.Config{
		Endpoints: map[string]config.ChainEndpoints{
			"ethereum": {
				"ep1": config.Endpoint{
					Provider:         "alchemy",
					Role:             "primary",
					Type:             "full",
					HTTPURL:          "http://ep1",
					CapacityLearning: &config.CapacityLearning{WindowSeconds: 30}, // live value, differs from frozen
				},
			},
		},
	}
	valkeyClient := store.NewMockValkeyClient()
	ctx := context.Background()
	ep := cfg.Endpoints["ethereum"]["ep1"]

	// Seed an estimate frozen at 60s, with its cooldown already elapsed.
	if err := valkeyClient.SetCapacityEstimate(ctx, "ethereum", "ep1", store.CapacityEstimate{
		HasEstimate: true, MaxRequests: 10, IncreaseStep: 1, WindowSeconds: 60,
		LastDecreaseAt: time.Now().Add(-120 * time.Second),
	}); err != nil {
		t.Fatalf("SetCapacityEstimate failed: %v", err)
	}
	valkeyClient.IncrementCapacityCount(ctx, "ethereum", "ep1", 60)

	server := NewServer(cfg, valkeyClient, learningTestConfig())
	server.applyLearnedCapacityDecrease("ethereum", "ep1", ep)

	final, err := valkeyClient.GetCapacityEstimate(ctx, "ethereum", "ep1")
	if err != nil {
		t.Fatalf("GetCapacityEstimate failed: %v", err)
	}
	// The persisted estimate's WindowSeconds must stay frozen at the value it was seeded
	// with (60), not get re-synced to the live config value (30) on this decrease - the
	// window is documented as permanently frozen for the estimate's lifetime, not just
	// stable between reads.
	if final.WindowSeconds != 60 {
		t.Errorf("Expected the persisted estimate to keep its frozen WindowSeconds (60), got %d", final.WindowSeconds)
	}

	// A second decrease after another elapsed cooldown must still persist the original
	// frozen window, not the live one - the freeze must survive across repeated decreases.
	valkeyClient.SetCapacityEstimate(ctx, "ethereum", "ep1", store.CapacityEstimate{
		HasEstimate: true, MaxRequests: final.MaxRequests, IncreaseStep: final.IncreaseStep, WindowSeconds: final.WindowSeconds,
		LastDecreaseAt: time.Now().Add(-120 * time.Second),
	})
	valkeyClient.IncrementCapacityCount(ctx, "ethereum", "ep1", 60)
	server.applyLearnedCapacityDecrease("ethereum", "ep1", ep)

	final2, err := valkeyClient.GetCapacityEstimate(ctx, "ethereum", "ep1")
	if err != nil {
		t.Fatalf("GetCapacityEstimate failed: %v", err)
	}
	if final2.WindowSeconds != 60 {
		t.Errorf("Expected the frozen window to survive a second decrease (60), got %d", final2.WindowSeconds)
	}
}
