package store

import (
	"context"
	"encoding/json"
	"sync"
	"time"
)

// MockRedisClient is a mock implementation of RedisClientIface for testing.
// It supports in-memory endpoint status storage and is safe for concurrent use.
type MockRedisClient struct {
	rateLimitStates map[string]*RateLimitState
	requestCounts   map[string]map[string]map[string][3]int64 // [0]=24h, [1]=1m, [2]=all
	statuses        map[string]*EndpointStatus
	values          map[string]string
	mu              sync.RWMutex
}

// NewMockRedisClient creates a new MockRedisClient with empty state.
func NewMockRedisClient() *MockRedisClient {
	return &MockRedisClient{
		rateLimitStates: make(map[string]*RateLimitState),
		requestCounts:   make(map[string]map[string]map[string][3]int64),
		statuses:        make(map[string]*EndpointStatus),
		values:          make(map[string]string),
	}
}

// GetEndpointStatus returns the status for a given chain and endpoint.
func (m *MockRedisClient) GetEndpointStatus(_ context.Context, chain, endpointID string) (*EndpointStatus, error) {
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
func (m *MockRedisClient) UpdateEndpointStatus(_ context.Context, chain, endpointID string, status EndpointStatus) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := chain + ":" + endpointID
	m.statuses[key] = &status
	return nil
}

// IncrementRequestCount is a stub for incrementing request counts.
func (m *MockRedisClient) IncrementRequestCount(ctx context.Context, chain, endpoint string, requestType string) error {
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
func (m *MockRedisClient) GetCombinedRequestCounts(ctx context.Context, chain, endpoint string) (int64, int64, int64, error) {
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
func (m *MockRedisClient) Ping(_ context.Context) error {
	return nil
}

// Close is a stub for closing the client.
func (m *MockRedisClient) Close() error {
	return nil
}

// Set sets a value in the mock as a JSON string
func (m *MockRedisClient) Set(ctx context.Context, key string, value interface{}, expiration time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var strVal string
	switch v := value.(type) {
	case string:
		strVal = v
	case []byte:
		strVal = string(v)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		strVal = string(b)
	}
	m.values[key] = strVal
	return nil
}

// Get gets a JSON string value from the mock
func (m *MockRedisClient) Get(ctx context.Context, key string) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	val, ok := m.values[key]
	if !ok {
		return "", nil // Simulate redis.Nil
	}
	return val, nil
}

// Del deletes a key in the mock
func (m *MockRedisClient) Del(ctx context.Context, keys ...string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, key := range keys {
		delete(m.values, key)
	}
	return nil
}

// GetRequestCounts is a stub for returning request counts (matches real RedisClient signature).
func (m *MockRedisClient) GetRequestCounts(ctx context.Context, chain, endpoint, requestType string) (int64, int64, int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if c, ok := m.requestCounts[chain][endpoint][requestType]; ok {
		return c[0], c[1], c[2], nil
	}
	return 0, 0, 0, nil
}

// GetRateLimitState returns the rate limit state for a given chain and endpoint
func (m *MockRedisClient) GetRateLimitState(_ context.Context, chain, endpoint string) (*RateLimitState, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	key := chain + ":" + endpoint
	state, ok := m.rateLimitStates[key]
	if !ok {
		return &RateLimitState{
			RateLimited:        false,
			RecoveryAttempts:   0,
			LastRecoveryCheck:  time.Time{},
			ConsecutiveSuccess: 0,
		}, nil
	}
	return state, nil
}

// SetRateLimitState sets the rate limit state for a given chain and endpoint
func (m *MockRedisClient) SetRateLimitState(_ context.Context, chain, endpoint string, state RateLimitState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := chain + ":" + endpoint
	m.rateLimitStates[key] = &state
	return nil
}

// PopulateStatuses allows tests to pre-populate endpoint statuses in the mock.
func (m *MockRedisClient) PopulateStatuses(statuses map[string]*EndpointStatus) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, v := range statuses {
		m.statuses[k] = v
	}
}
