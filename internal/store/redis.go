package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	// Key prefixes for Redis storage
	healthPrefix   = "health:"
	metricsPrefix  = "metrics:"
	proxyRequests  = "proxy_requests"
	healthRequests = "health_requests"
	requests24hKey = "requests_24h"
	requests1mKey  = "requests_1m"
	requestsAllKey = "requests_all"
)

// EndpointStatus represents the health status and metrics of an endpoint.
// It contains information about the endpoint's health, protocol support, and request counts.
type EndpointStatus struct {
	LastHealthCheck  time.Time `json:"last_health_check"`  // When the last health check was performed
	Requests24h      int64     `json:"requests_24h"`       // Number of requests in the last 24 hours
	Requests1Month   int64     `json:"requests_1_month"`   // Number of requests in the last month
	RequestsLifetime int64     `json:"requests_lifetime"`  // Total number of requests since start

	// Protocol support and health flags
	HasHTTP     bool `json:"has_http"`     // Whether the endpoint supports HTTP/HTTPS
	HasWS       bool `json:"has_ws"`       // Whether the endpoint supports WebSocket
	HealthyHTTP bool `json:"healthy_http"` // Whether the HTTP endpoint is healthy
	HealthyWS   bool `json:"healthy_ws"`   // Whether the WebSocket endpoint is healthy
}

// NewEndpointStatus creates a new endpoint status with default values.
// All health flags are set to false and request counts are initialized to 0.
func NewEndpointStatus() EndpointStatus {
	return EndpointStatus{
		LastHealthCheck:  time.Now(),
		Requests24h:      0,
		Requests1Month:   0,
		RequestsLifetime: 0,
		HasHTTP:          false,
		HasWS:            false,
		HealthyHTTP:      false,
		HealthyWS:        false,
	}
}

// RedisClientIface defines the interface for Redis operations used by the server.
// This allows for mocking in tests and provides a clean separation of concerns.
// Only include methods actually used by the server.
type RedisClientIface interface {
	GetEndpointStatus(ctx context.Context, chain, endpoint string) (*EndpointStatus, error)
	UpdateEndpointStatus(ctx context.Context, chain, endpoint string, status EndpointStatus) error
	IncrementRequestCount(ctx context.Context, chain, endpoint string, requestType string) error
	GetCombinedRequestCounts(ctx context.Context, chain, endpoint string) (int64, int64, int64, error)
}

// RedisClient wraps the Redis client with our custom methods.
// It implements RedisClientIface and provides methods for storing and retrieving
// endpoint health status and request metrics.
type RedisClient struct {
	client *redis.Client
}

// NewRedisClient creates a new Redis client with optimized connection settings.
// It configures connection pooling, timeouts, and retry logic for production use.
func NewRedisClient(addr string, password string) *RedisClient {
	client := redis.NewClient(&redis.Options{
		Addr:            addr,
		Password:        password,
		MinIdleConns:    10,
		PoolSize:        100,
		PoolTimeout:     4 * time.Second,
		MaxRetries:      3,
		DialTimeout:     5 * time.Second,
		ReadTimeout:     3 * time.Second,
		WriteTimeout:    3 * time.Second,
		ConnMaxLifetime: 30 * time.Minute,
		ConnMaxIdleTime: 5 * time.Minute,
	})
	return &RedisClient{client: client}
}

// Ping checks the Redis connection by sending a PING command.
// Returns an error if the connection cannot be established.
func (r *RedisClient) Ping(ctx context.Context) error {
	return r.client.Ping(ctx).Err()
}

// Close closes the Redis connection and releases all resources.
func (r *RedisClient) Close() error {
	return r.client.Close()
}

// UpdateEndpointStatus updates the health status of an endpoint in Redis.
// The data is stored as JSON with the key pattern "health:{chain}:{endpoint}".
// The data has no expiration and persists until explicitly deleted.
func (r *RedisClient) UpdateEndpointStatus(ctx context.Context, chain, endpoint string, status EndpointStatus) error {
	key := healthPrefix + chain + ":" + endpoint
	data, err := json.Marshal(status)
	if err != nil {
		return err
	}
	return r.client.Set(ctx, key, data, 0).Err()
}

// GetEndpointStatus gets the health status of an endpoint from Redis.
// If the endpoint doesn't exist in Redis, it creates a new status with default values.
// Returns the endpoint status and any error that occurred during retrieval.
func (r *RedisClient) GetEndpointStatus(ctx context.Context, chain, endpoint string) (*EndpointStatus, error) {
	key := healthPrefix + chain + ":" + endpoint
	data, err := r.client.Get(ctx, key).Bytes()
	if err == redis.Nil {
		// Initialize new endpoint status if it doesn't exist
		status := NewEndpointStatus()
		if err := r.UpdateEndpointStatus(ctx, chain, endpoint, status); err != nil {
			return nil, err
		}
		return &status, nil
	}
	if err != nil {
		return nil, err
	}

	var status EndpointStatus
	if err := json.Unmarshal(data, &status); err != nil {
		return nil, err
	}
	return &status, nil
}

// IncrementRequestCount increments the request count for an endpoint.
// It maintains separate counters for 24-hour, 1-month, and lifetime requests.
// The 24-hour and 1-month counters have automatic expiration set.
func (r *RedisClient) IncrementRequestCount(ctx context.Context, chain, endpoint string, requestType string) error {
	key := metricsPrefix + chain + ":" + endpoint + ":" + requestType
	pipe := r.client.Pipeline()

	// Initialize counters if they don't exist
	pipe.Incr(ctx, key+":"+requests24hKey)
	pipe.Expire(ctx, key+":"+requests24hKey, 24*time.Hour)

	pipe.Incr(ctx, key+":"+requests1mKey)
	pipe.Expire(ctx, key+":"+requests1mKey, 30*24*time.Hour)

	pipe.Incr(ctx, key+":"+requestsAllKey)

	_, err := pipe.Exec(ctx)
	return err
}

// GetRequestCounts gets the request counts for an endpoint.
// Returns the 24-hour, 1-month, and lifetime request counts.
// If any counter doesn't exist, it returns 0 for that counter.
func (r *RedisClient) GetRequestCounts(ctx context.Context, chain, endpoint string, requestType string) (int64, int64, int64, error) {
	key := metricsPrefix + chain + ":" + endpoint + ":" + requestType
	pipe := r.client.Pipeline()

	requests24h := pipe.Get(ctx, key+":"+requests24hKey)
	requests1m := pipe.Get(ctx, key+":"+requests1mKey)
	requestsAll := pipe.Get(ctx, key+":"+requestsAllKey)

	_, err := pipe.Exec(ctx)
	if err != nil && err != redis.Nil {
		return 0, 0, 0, err
	}

	var r24h, r1m, rAll int64
	if requests24h.Val() != "" {
		r24h, _ = requests24h.Int64()
	}
	if requests1m.Val() != "" {
		r1m, _ = requests1m.Int64()
	}
	if requestsAll.Val() != "" {
		rAll, _ = requestsAll.Int64()
	}

	return r24h, r1m, rAll, nil
}

// GetCombinedRequestCounts gets the combined request counts (proxy + health) for an endpoint.
// This is useful for monitoring total endpoint usage including health check requests.
// Returns the combined 24-hour, 1-month, and lifetime request counts.
func (r *RedisClient) GetCombinedRequestCounts(ctx context.Context, chain, endpoint string) (int64, int64, int64, error) {
	// Get proxy request counts
	p24h, p1m, pAll, err := r.GetRequestCounts(ctx, chain, endpoint, proxyRequests)
	if err != nil {
		p24h, p1m, pAll = 0, 0, 0
	}

	// Get health request counts
	h24h, h1m, hAll, err := r.GetRequestCounts(ctx, chain, endpoint, healthRequests)
	if err != nil {
		h24h, h1m, hAll = 0, 0, 0
	}

	// Return combined counts
	return p24h + h24h, p1m + h1m, pAll + hAll, nil
}

// MockRedisClient wraps the mock implementation and implements RedisClientIface
type MockRedisClient struct {
	*mockRedisClient
}

// Exported for use in other packages
func NewMockRedisClient() *MockRedisClient {
	return &MockRedisClient{
		mockRedisClient: &mockRedisClient{
			data: make(map[string]string),
		},
	}
}

// mockRedisClient implements a mock Redis client for testing
type mockRedisClient struct {
	data map[string]string
	mu   sync.RWMutex
}

// Set implements the Redis Set command
func (m *mockRedisClient) Set(ctx context.Context, key string, value interface{}, expiration time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var data string
	switch v := value.(type) {
	case string:
		data = v
	case []byte:
		data = string(v)
	default:
		if jsonData, err := json.Marshal(value); err == nil {
			data = string(jsonData)
		} else {
			data = fmt.Sprintf("%v", value)
		}
	}
	m.data[key] = data
	return nil
}

// Get implements the Redis Get command
func (m *mockRedisClient) Get(ctx context.Context, key string) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if data, exists := m.data[key]; exists {
		return data, nil
	}
	return "", fmt.Errorf("key not found")
}

// Incr implements the Redis Incr command
func (m *mockRedisClient) Incr(ctx context.Context, key string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	current := int64(0)
	if data, exists := m.data[key]; exists {
		if err := json.Unmarshal([]byte(data), &current); err != nil {
			current = 0
		}
	}
	current++
	newData, _ := json.Marshal(current)
	m.data[key] = string(newData)
	return current, nil
}

// Expire implements the Redis Expire command
func (m *mockRedisClient) Expire(ctx context.Context, key string, expiration time.Duration) error {
	return nil
}

// Del implements the Redis Del command
func (m *mockRedisClient) Del(ctx context.Context, keys ...string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, key := range keys {
		delete(m.data, key)
	}
	return nil
}

// GetCombinedRequestCounts gets the combined request counts (proxy + health) for an endpoint
func (m *MockRedisClient) GetCombinedRequestCounts(ctx context.Context, chain, endpoint string) (int64, int64, int64, error) {
	// Get proxy request counts
	p24h, p1m, pAll, err := m.GetRequestCounts(ctx, chain, endpoint, proxyRequests)
	if err != nil {
		p24h, p1m, pAll = 0, 0, 0
	}

	// Get health request counts
	h24h, h1m, hAll, err := m.GetRequestCounts(ctx, chain, endpoint, healthRequests)
	if err != nil {
		h24h, h1m, hAll = 0, 0, 0
	}

	// Return combined counts
	return p24h + h24h, p1m + h1m, pAll + hAll, nil
}

// GetRequestCounts gets the request counts for an endpoint from the mock Redis
func (m *MockRedisClient) GetRequestCounts(ctx context.Context, chain, endpoint string, requestType string) (int64, int64, int64, error) {
	key := metricsPrefix + chain + ":" + endpoint + ":" + requestType
	pipe := m.Pipeline()

	requests24h := pipe.Get(ctx, key+":"+requests24hKey)
	requests1m := pipe.Get(ctx, key+":"+requests1mKey)
	requestsAll := pipe.Get(ctx, key+":"+requestsAllKey)

	_, err := pipe.Exec(ctx)
	if err != nil {
		return 0, 0, 0, err
	}

	var r24h, r1m, rAll int64
	if requests24h.Val() != "" {
		r24h, _ = requests24h.Int64()
	}
	if requests1m.Val() != "" {
		r1m, _ = requests1m.Int64()
	}
	if requestsAll.Val() != "" {
		rAll, _ = requestsAll.Int64()
	}

	return r24h, r1m, rAll, nil
}

// GetEndpointStatus gets the health status of an endpoint from the mock Redis
func (m *MockRedisClient) GetEndpointStatus(ctx context.Context, chain, endpoint string) (*EndpointStatus, error) {
	key := healthPrefix + chain + ":" + endpoint
	data, err := m.Get(ctx, key)
	if err != nil {
		// Initialize new endpoint status if it doesn't exist
		status := NewEndpointStatus()
		if err := m.UpdateEndpointStatus(ctx, chain, endpoint, status); err != nil {
			return nil, err
		}
		return &status, nil
	}

	var status EndpointStatus
	if err := json.Unmarshal([]byte(data), &status); err != nil {
		return nil, err
	}
	return &status, nil
}

// UpdateEndpointStatus updates the health status of an endpoint in the mock Redis
func (m *MockRedisClient) UpdateEndpointStatus(ctx context.Context, chain, endpoint string, status EndpointStatus) error {
	key := healthPrefix + chain + ":" + endpoint
	data, err := json.Marshal(status)
	if err != nil {
		return err
	}
	return m.Set(ctx, key, data, 0)
}

// IncrementRequestCount increments the request count for an endpoint in the mock Redis
func (m *MockRedisClient) IncrementRequestCount(ctx context.Context, chain, endpoint string, requestType string) error {
	key := metricsPrefix + chain + ":" + endpoint + ":" + requestType
	pipe := m.Pipeline()

	// Initialize counters if they don't exist
	pipe.Incr(ctx, key+":"+requests24hKey)
	pipe.Expire(ctx, key+":"+requests24hKey, 24*time.Hour)

	pipe.Incr(ctx, key+":"+requests1mKey)
	pipe.Expire(ctx, key+":"+requests1mKey, 30*24*time.Hour)

	pipe.Incr(ctx, key+":"+requestsAllKey)

	_, err := pipe.Exec(ctx)
	return err
}

// Pipeline returns a mock pipeline
func (m *MockRedisClient) Pipeline() *MockPipeline {
	return &MockPipeline{client: m}
}

// MockPipeline implements a mock Redis pipeline
type MockPipeline struct {
	client *MockRedisClient
	cmds   []interface{}
}

func (p *MockPipeline) Incr(ctx context.Context, key string) *MockIntCmd {
	cmd := &MockIntCmd{key: key, client: p.client}
	p.cmds = append(p.cmds, cmd)
	return cmd
}

func (p *MockPipeline) Expire(ctx context.Context, key string, expiration time.Duration) *MockStatusCmd {
	cmd := &MockStatusCmd{key: key}
	p.cmds = append(p.cmds, cmd)
	return cmd
}

func (p *MockPipeline) Get(ctx context.Context, key string) *MockStringCmd {
	cmd := &MockStringCmd{key: key, client: p.client}
	p.cmds = append(p.cmds, cmd)
	return cmd
}

func (p *MockPipeline) Exec(ctx context.Context) ([]interface{}, error) {
	for _, cmd := range p.cmds {
		switch c := cmd.(type) {
		case *MockIntCmd:
			c.Execute()
		case *MockStatusCmd:
			c.Execute()
		case *MockStringCmd:
			c.Execute()
		}
	}
	return p.cmds, nil
}

// MockIntCmd represents a mock Redis integer command
type MockIntCmd struct {
	key    string
	client *MockRedisClient
	val    int64
	err    error
}

func (c *MockIntCmd) Execute() {
	c.val, c.err = c.client.Incr(context.Background(), c.key)
}

func (c *MockIntCmd) Int64() (int64, error) {
	return c.val, c.err
}

// MockStatusCmd represents a mock Redis status command
type MockStatusCmd struct {
	key string
	val string
	err error
}

func (c *MockStatusCmd) Execute() {
	c.val = "OK"
	c.err = nil
}

func (c *MockStatusCmd) Result() (string, error) {
	return c.val, c.err
}

// MockStringCmd represents a mock Redis string command
type MockStringCmd struct {
	key    string
	client *MockRedisClient
	val    string
	err    error
}

func (c *MockStringCmd) Execute() {
	c.val, c.err = c.client.Get(context.Background(), c.key)
}

func (c *MockStringCmd) Result() (string, error) {
	return c.val, c.err
}

func (c *MockStringCmd) Val() string {
	return c.val
}

func (c *MockStringCmd) Int64() (int64, error) {
	if c.val == "" {
		return 0, nil
	}
	var val int64
	err := json.Unmarshal([]byte(c.val), &val)
	return val, err
}
