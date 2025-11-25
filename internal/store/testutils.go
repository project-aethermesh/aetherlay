package store

import (
	"context"
	"sync"
	"time"
)

// MockValkeyClient is a mock implementation of ValkeyClientIface for testing.
// It supports in-memory endpoint status storage and is safe for concurrent use.
type MockValkeyClient struct {
	rateLimitStates map[string]*RateLimitState
	requestCounts   map[string]map[string]map[string][3]int64 // [0]=24h, [1]=1m, [2]=all
	statuses        map[string]*EndpointStatus
	values          map[string]string // Generic key-value storage for Set/Get
	mu              sync.RWMutex
}

// NewMockValkeyClient creates a new MockValkeyClient with empty state.
func NewMockValkeyClient() *MockValkeyClient {
	return &MockValkeyClient{
		rateLimitStates: make(map[string]*RateLimitState),
		requestCounts:   make(map[string]map[string]map[string][3]int64),
		statuses:        make(map[string]*EndpointStatus),
		values:          make(map[string]string),
	}
}

// GetEndpointStatus returns the status for a given chain and endpoint.
func (m *MockValkeyClient) GetEndpointStatus(_ context.Context, chain, endpointID string) (*EndpointStatus, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	key := chain + ":" + endpointID
	status, ok := m.statuses[key]
	if !ok {
		return &EndpointStatus{}, nil
	}
	return status, nil
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

// SetRateLimitState sets the rate limit state for a given chain and endpoint
func (m *MockValkeyClient) SetRateLimitState(_ context.Context, chain, endpoint string, state RateLimitState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := chain + ":" + endpoint
	m.rateLimitStates[key] = &state
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
