package store

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// MockValkeyClient is a mock implementation of ValkeyClientIface for testing.
// It supports in-memory endpoint status storage and is safe for concurrent use.
type MockValkeyClient struct {
	rateLimitStates   map[string]*RateLimitState
	customProbeStates map[string]*CustomProbeState
	customProbeGates  map[string]time.Time                      // "chain:endpoint" -> when the gate expires
	requestCounts     map[string]map[string]map[string][3]int64 // [0]=24h, [1]=1m, [2]=all
	capacityCounts    map[string]map[int64]int64                // "chain:endpoint" -> bucket -> count
	capacityEstimates map[string]*CapacityEstimate              // "chain:endpoint" -> learned estimate
	statuses          map[string]*EndpointStatus
	values            map[string]string // Generic key-value storage for Set/Get
	mu                sync.RWMutex

	// NowFunc lets tests deterministically simulate capacity-window rollover
	// without a real sleep. Defaults to time.Now.
	NowFunc func() time.Time
}

// NewMockValkeyClient creates a new MockValkeyClient with empty state.
func NewMockValkeyClient() *MockValkeyClient {
	return &MockValkeyClient{
		rateLimitStates:   make(map[string]*RateLimitState),
		customProbeStates: make(map[string]*CustomProbeState),
		customProbeGates:  make(map[string]time.Time),
		requestCounts:     make(map[string]map[string]map[string][3]int64),
		capacityCounts:    make(map[string]map[int64]int64),
		capacityEstimates: make(map[string]*CapacityEstimate),
		statuses:          make(map[string]*EndpointStatus),
		values:            make(map[string]string),
		NowFunc:           time.Now,
	}
}

// GetEndpointStatus returns the status for a given chain and endpoint. It returns a copy,
// not the stored pointer, matching the real ValkeyClient (which always hands back a
// freshly unmarshaled value). Callers that mutate the fields of an EndpointStatus they
// got from a prior Get (checker.go, server.go's updateEndpointHealthState) always
// explicitly write it back via UpdateEndpointStatus; if Get returned the live stored
// pointer instead, that in-place mutation would race with any concurrent reader of the
// same endpoint's status, such as a test polling health state from another goroutine.
func (m *MockValkeyClient) GetEndpointStatus(_ context.Context, chain, endpointID string) (*EndpointStatus, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	key := chain + ":" + endpointID
	status, ok := m.statuses[key]
	if !ok {
		return &EndpointStatus{}, nil
	}
	statusCopy := *status
	return &statusCopy, nil
}

// UpdateEndpointStatus sets the status for a given chain and endpoint.
func (m *MockValkeyClient) UpdateEndpointStatus(_ context.Context, chain, endpointID string, status EndpointStatus) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := chain + ":" + endpointID
	m.statuses[key] = &status
	return nil
}

// IncrementRequestCount is a stub for incrementing request counts.
func (m *MockValkeyClient) IncrementRequestCount(ctx context.Context, chain, endpoint string, requestType string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.requestCounts[chain]; !ok {
		m.requestCounts[chain] = make(map[string]map[string][3]int64)
	}
	if _, ok := m.requestCounts[chain][endpoint]; !ok {
		m.requestCounts[chain][endpoint] = make(map[string][3]int64)
	}
	counts := m.requestCounts[chain][endpoint][requestType]
	counts[0]++ // 24h
	counts[1]++ // 1m
	counts[2]++ // all
	m.requestCounts[chain][endpoint][requestType] = counts
	return nil
}

// GetCombinedRequestCounts is a stub for returning request counts.
func (m *MockValkeyClient) GetCombinedRequestCounts(ctx context.Context, chain, endpoint string) (int64, int64, int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var total [3]int64
	for _, reqType := range []string{"proxy_requests", "health_requests"} {
		if c, ok := m.requestCounts[chain][endpoint][reqType]; ok {
			total[0] += c[0]
			total[1] += c[1]
			total[2] += c[2]
		}
	}
	return total[0], total[1], total[2], nil
}

// Ping is a stub for checking connectivity.
func (m *MockValkeyClient) Ping(_ context.Context) error {
	return nil
}

// Close is a stub for closing the client.
func (m *MockValkeyClient) Close() error {
	return nil
}

// GetRequestCounts is a stub for returning request counts (matches real ValkeyClient signature).
func (m *MockValkeyClient) GetRequestCounts(ctx context.Context, chain, endpoint, requestType string) (int64, int64, int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if c, ok := m.requestCounts[chain][endpoint][requestType]; ok {
		return c[0], c[1], c[2], nil
	}
	return 0, 0, 0, nil
}

// GetRateLimitState returns the rate limit state for a given chain and endpoint
func (m *MockValkeyClient) GetRateLimitState(_ context.Context, chain, endpoint string) (*RateLimitState, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	key := chain + ":" + endpoint
	state, ok := m.rateLimitStates[key]
	if !ok {
		return &RateLimitState{
			ConsecutiveSuccess: 0,
			CurrentBackoff:     0,
			FirstRateLimited:   time.Time{},
			LastRecoveryCheck:  time.Time{},
			RateLimited:        false,
			RecoveryAttempts:   0,
		}, nil
	}
	return state, nil
}

// GetCustomProbeState returns the custom probe state for a given chain and endpoint, if
// one has been set. A nil result (with a nil error) means no custom probe method is
// currently active, matching the real ValkeyClient's behavior on a cache miss. It returns
// a copy, not the stored pointer, for the same reason GetEndpointStatus does: exposing
// the map-owned pointer after releasing m.mu would let a caller mutate the stored state
// without synchronization, and would behave differently from the real ValkeyClient, which
// JSON-decodes a fresh value on every read.
func (m *MockValkeyClient) GetCustomProbeState(_ context.Context, chain, endpoint string) (*CustomProbeState, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	key := chain + ":" + endpoint
	state := m.customProbeStates[key]
	if state == nil {
		return nil, nil
	}
	stateCopy := *state
	return &stateCopy, nil
}

// SetCustomProbeState sets the custom probe state for a given chain and endpoint.
func (m *MockValkeyClient) SetCustomProbeState(_ context.Context, chain, endpoint string, state CustomProbeState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := chain + ":" + endpoint
	m.customProbeStates[key] = &state
	return nil
}

// ClearCustomProbeState removes the custom probe state for a given chain and endpoint.
func (m *MockValkeyClient) ClearCustomProbeState(_ context.Context, chain, endpoint string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := chain + ":" + endpoint
	delete(m.customProbeStates, key)
	return nil
}

// TryAcquireCustomProbeGate mirrors the real ValkeyClient's SET NX EX gate: it atomically
// (under the mock's own lock) checks whether the gate for chain:endpoint is currently
// held and, if not, claims it for ttl and returns true. Concurrent callers under -race
// exercise the same lock, so this only "succeeds" for exactly one caller per window, same
// as SET NX would on a real Valkey server.
func (m *MockValkeyClient) TryAcquireCustomProbeGate(_ context.Context, chain, endpoint string, ttl time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := chain + ":" + endpoint
	now := m.NowFunc()
	if expiresAt, held := m.customProbeGates[key]; held && now.Before(expiresAt) {
		return false, nil
	}
	m.customProbeGates[key] = now.Add(ttl)
	return true, nil
}

// CleanupStaleEndpoints is a no-op stub for tests; returns 0 deleted and no error.
func (m *MockValkeyClient) CleanupStaleEndpoints(_ context.Context, _ map[string][]string) (int, error) {
	return 0, nil
}

// SetRateLimitState sets the rate limit state for a given chain and endpoint
func (m *MockValkeyClient) SetRateLimitState(_ context.Context, chain, endpoint string, state RateLimitState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := chain + ":" + endpoint
	m.rateLimitStates[key] = &state
	return nil
}

// IncrementCapacityCount increments the mock's in-memory capacity counter for the
// endpoint's current fixed window (bucketed by NowFunc().Unix()/windowSeconds, mirroring
// the real ValkeyClient's bucketing) and returns the new count.
func (m *MockValkeyClient) IncrementCapacityCount(_ context.Context, chain, endpoint string, windowSeconds int) (int64, error) {
	if windowSeconds <= 0 {
		return 0, fmt.Errorf("IncrementCapacityCount: windowSeconds must be positive, got %d", windowSeconds)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := chain + ":" + endpoint
	bucket := m.NowFunc().Unix() / int64(windowSeconds)
	if _, ok := m.capacityCounts[key]; !ok {
		m.capacityCounts[key] = make(map[int64]int64)
	}
	m.capacityCounts[key][bucket]++
	return m.capacityCounts[key][bucket], nil
}

// GetCapacityCount returns the mock's in-memory capacity counter for the endpoint's
// current fixed window, or 0 if nothing has been recorded in that window yet.
func (m *MockValkeyClient) GetCapacityCount(_ context.Context, chain, endpoint string, windowSeconds int) (int64, error) {
	if windowSeconds <= 0 {
		return 0, fmt.Errorf("GetCapacityCount: windowSeconds must be positive, got %d", windowSeconds)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	key := chain + ":" + endpoint
	bucket := m.NowFunc().Unix() / int64(windowSeconds)
	if buckets, ok := m.capacityCounts[key]; ok {
		return buckets[bucket], nil
	}
	return 0, nil
}

// GetCapacityEstimate returns the mock's in-memory learned capacity estimate for an
// endpoint, or a zero-value estimate (HasEstimate: false) if none has been recorded.
func (m *MockValkeyClient) GetCapacityEstimate(_ context.Context, chain, endpoint string) (*CapacityEstimate, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	key := chain + ":" + endpoint
	estimate, ok := m.capacityEstimates[key]
	if !ok {
		return &CapacityEstimate{}, nil
	}
	return estimate, nil
}

// SetCapacityEstimate stores the mock's in-memory learned capacity estimate for an endpoint.
func (m *MockValkeyClient) SetCapacityEstimate(_ context.Context, chain, endpoint string, estimate CapacityEstimate) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := chain + ":" + endpoint
	m.capacityEstimates[key] = &estimate
	return nil
}

// PopulateStatuses allows tests to pre-populate endpoint statuses in the mock.
func (m *MockValkeyClient) PopulateStatuses(statuses map[string]*EndpointStatus) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, v := range statuses {
		m.statuses[k] = v
	}
}

// Del is a helper method for tests to clean up data from the mock.
// Since each test creates a new MockValkeyClient instance, this is effectively a no-op.
// It's provided for API compatibility with test cleanup code.
func (m *MockValkeyClient) Del(_ context.Context, keys ...string) error {
	// No-op: Each test creates a fresh MockValkeyClient, so cleanup is not needed
	return nil
}

// Set is a helper method used by some components (like health checker) that don't use the full interface.
// It stores a value as a string in a simple key-value map.
func (m *MockValkeyClient) Set(_ context.Context, key string, value any, _ time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var strVal string
	switch v := value.(type) {
	case string:
		strVal = v
	case []byte:
		strVal = string(v)
	default:
		return nil // Unsupported type for mock
	}
	m.values[key] = strVal
	return nil
}

// Get is a helper method used by some components (like health checker) that don't use the full interface.
// It retrieves a value from the simple key-value map.
func (m *MockValkeyClient) Get(_ context.Context, key string) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	val, ok := m.values[key]
	if !ok {
		return "", nil // Return empty for non-existent keys
	}
	return val, nil
}
