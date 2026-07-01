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
