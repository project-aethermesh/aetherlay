package store

import (
	"context"
	"testing"
	"time"

	"aetherlay/internal/config"
)

func TestApplyCapacityDecrease(t *testing.T) {
	params := config.CapacityLearning{DecreaseFactor: 0.5, MinEstimate: 1, IncreaseInterval: 60, WindowSeconds: 60}
	now := time.Now()

	tests := []struct {
		name           string
		prior          CapacityEstimate
		effectiveNow   int64
		observedCount  int64
		windowSeconds  int
		expectedMax    int64
		expectedStep   int64
		expectedWindow int
	}{
		{
			name:           "no prior estimate seeds from observed count",
			prior:          CapacityEstimate{},
			effectiveNow:   0,
			observedCount:  100,
			windowSeconds:  10,
			expectedMax:    50,
			expectedStep:   5,
			expectedWindow: 10,
		},
		{
			name:           "prior lower than observed - trust the prior (more conservative)",
			prior:          CapacityEstimate{HasEstimate: true, MaxRequests: 40},
			effectiveNow:   40,
			observedCount:  100,
			windowSeconds:  10,
			expectedMax:    20,
			expectedStep:   2,
			expectedWindow: 10,
		},
		{
			name:           "observed lower than prior effective ceiling - trust the observed count",
			prior:          CapacityEstimate{HasEstimate: true, MaxRequests: 150},
			effectiveNow:   150,
			observedCount:  80,
			windowSeconds:  10,
			expectedMax:    40,
			expectedStep:   4,
			expectedWindow: 10,
		},
		{
			name:           "floor enforcement - result clamped to MinEstimate",
			prior:          CapacityEstimate{},
			effectiveNow:   0,
			observedCount:  1,
			windowSeconds:  10,
			expectedMax:    1,
			expectedStep:   1,
			expectedWindow: 10,
		},
		{
			name:           "zero observed count clamps to floor",
			prior:          CapacityEstimate{},
			effectiveNow:   0,
			observedCount:  0,
			windowSeconds:  10,
			expectedMax:    1,
			expectedStep:   1,
			expectedWindow: 10,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ApplyCapacityDecrease(tt.prior, tt.effectiveNow, tt.observedCount, tt.windowSeconds, params, now)

			if !result.HasEstimate {
				t.Error("Expected HasEstimate to be true after a decrease")
			}
			if result.MaxRequests != tt.expectedMax {
				t.Errorf("MaxRequests = %d, want %d", result.MaxRequests, tt.expectedMax)
			}
			if result.IncreaseStep != tt.expectedStep {
				t.Errorf("IncreaseStep = %d, want %d", result.IncreaseStep, tt.expectedStep)
			}
			if result.WindowSeconds != tt.expectedWindow {
				t.Errorf("WindowSeconds = %d, want %d", result.WindowSeconds, tt.expectedWindow)
			}
			if !result.LastDecreaseAt.Equal(now) {
				t.Errorf("LastDecreaseAt = %v, want %v", result.LastDecreaseAt, now)
			}
		})
	}
}

func TestShouldApplyCapacityDecrease(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name     string
		estimate CapacityEstimate
		window   int
		expected bool
	}{
		{"never decreased", CapacityEstimate{HasEstimate: false}, 10, true},
		{"within cooldown", CapacityEstimate{HasEstimate: true, LastDecreaseAt: now.Add(-5 * time.Second)}, 10, false},
		{"exactly at cooldown boundary", CapacityEstimate{HasEstimate: true, LastDecreaseAt: now.Add(-10 * time.Second)}, 10, true},
		{"after cooldown elapsed", CapacityEstimate{HasEstimate: true, LastDecreaseAt: now.Add(-30 * time.Second)}, 10, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ShouldApplyCapacityDecrease(tt.estimate, tt.window, now); got != tt.expected {
				t.Errorf("ShouldApplyCapacityDecrease() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestEffectiveMaxRequests(t *testing.T) {
	now := time.Now()
	params := config.CapacityLearning{IncreaseInterval: 60}

	tests := []struct {
		name     string
		estimate CapacityEstimate
		params   config.CapacityLearning
		expected int64
	}{
		{
			name:     "no estimate returns 0",
			estimate: CapacityEstimate{HasEstimate: false},
			params:   params,
			expected: 0,
		},
		{
			name:     "zero elapsed returns base ceiling unchanged",
			estimate: CapacityEstimate{HasEstimate: true, MaxRequests: 50, IncreaseStep: 5, LastDecreaseAt: now},
			params:   params,
			expected: 50,
		},
		{
			name:     "one interval elapsed adds one step",
			estimate: CapacityEstimate{HasEstimate: true, MaxRequests: 50, IncreaseStep: 5, LastDecreaseAt: now.Add(-60 * time.Second)},
			params:   params,
			expected: 55,
		},
		{
			name:     "multiple intervals elapsed adds proportional steps",
			estimate: CapacityEstimate{HasEstimate: true, MaxRequests: 50, IncreaseStep: 5, LastDecreaseAt: now.Add(-185 * time.Second)},
			params:   params,
			expected: 65, // floor(185/60) = 3 steps * 5 = 15
		},
		{
			name:     "IncreaseInterval <= 0 guard returns base ceiling unchanged",
			estimate: CapacityEstimate{HasEstimate: true, MaxRequests: 50, IncreaseStep: 5, LastDecreaseAt: now.Add(-1 * time.Hour)},
			params:   config.CapacityLearning{IncreaseInterval: 0},
			expected: 50,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EffectiveMaxRequests(tt.estimate, tt.params, now); got != tt.expected {
				t.Errorf("EffectiveMaxRequests() = %d, want %d", got, tt.expected)
			}
		})
	}
}

func TestGetAndSetCapacityEstimateMock(t *testing.T) {
	client := NewMockValkeyClient()
	ctx := context.Background()
	chain := "ethereum"
	endpoint := "ep1"

	// Absent estimate returns a zero-value, non-nil result.
	estimate, err := client.GetCapacityEstimate(ctx, chain, endpoint)
	if err != nil {
		t.Fatalf("GetCapacityEstimate failed: %v", err)
	}
	if estimate.HasEstimate {
		t.Error("Expected HasEstimate to be false for an unseen endpoint")
	}

	stored := CapacityEstimate{
		HasEstimate:    true,
		MaxRequests:    42,
		IncreaseStep:   4,
		WindowSeconds:  60,
		LastDecreaseAt: time.Now(),
	}
	if err := client.SetCapacityEstimate(ctx, chain, endpoint, stored); err != nil {
		t.Fatalf("SetCapacityEstimate failed: %v", err)
	}

	retrieved, err := client.GetCapacityEstimate(ctx, chain, endpoint)
	if err != nil {
		t.Fatalf("GetCapacityEstimate failed: %v", err)
	}
	if !retrieved.HasEstimate || retrieved.MaxRequests != 42 || retrieved.IncreaseStep != 4 {
		t.Errorf("Expected stored estimate to round-trip, got %+v", retrieved)
	}
}
