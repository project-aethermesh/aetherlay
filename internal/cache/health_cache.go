package cache

import (
	"sync"
	"time"

	"aetherlay/internal/store"
)

// cacheEntry represents a single cache entry with its timestamp
type cacheEntry struct {
	status    *store.EndpointStatus
	timestamp time.Time
}

// HealthCache provides thread-safe caching of endpoint health status with TTL
type HealthCache struct {
	entries map[string]*cacheEntry // key: "chain:endpoint"
	mu      sync.RWMutex
	ttl     time.Duration
}

// NewHealthCache creates a new health status cache with the specified TTL
func NewHealthCache(ttl time.Duration) *HealthCache {
	return &HealthCache{
		entries: make(map[string]*cacheEntry),
		ttl:     ttl,
	}
}

// Get retrieves a cached endpoint status if it exists and is fresh
// Returns the status and true if found and fresh, nil and false otherwise
func (hc *HealthCache) Get(chain, endpoint string) (*store.EndpointStatus, bool) {
	key := chain + ":" + endpoint

	hc.mu.RLock()
	entry, exists := hc.entries[key]
	hc.mu.RUnlock()

	if !exists {
		return nil, false
	}

	// Check if entry is still fresh
	if time.Since(entry.timestamp) > hc.ttl {
		// Entry expired, remove it
		hc.Invalidate(chain, endpoint)
		return nil, false
	}

	return entry.status, true
}

// Set updates the cache with a new endpoint status
func (hc *HealthCache) Set(chain, endpoint string, status *store.EndpointStatus) {
	key := chain + ":" + endpoint

	hc.mu.Lock()
	hc.entries[key] = &cacheEntry{
		status:    status,
		timestamp: time.Now(),
	}
	hc.mu.Unlock()
}

// Invalidate removes a specific endpoint from the cache
func (hc *HealthCache) Invalidate(chain, endpoint string) {
	key := chain + ":" + endpoint

	hc.mu.Lock()
	delete(hc.entries, key)
	hc.mu.Unlock()
}

// Clear removes all entries from the cache
func (hc *HealthCache) Clear() {
	hc.mu.Lock()
	hc.entries = make(map[string]*cacheEntry)
	hc.mu.Unlock()
}
