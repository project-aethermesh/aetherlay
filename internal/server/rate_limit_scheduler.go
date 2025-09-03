package server

import (
	"context"
	"net/http"
	"sync"
	"time"

	"aetherlay/internal/config"
	"aetherlay/internal/helpers"
	"aetherlay/internal/store"

	"github.com/rs/zerolog/log"
)

// RateLimitScheduler manages recovery checks for rate-limited endpoints
type RateLimitScheduler struct {
	config      *config.Config
	redisClient store.RedisClientIface

	// Active monitoring tracking
	activeMonitoring map[string]bool // key: chain:endpoint
	mu               sync.RWMutex
}

// NewRateLimitScheduler creates a new rate limit scheduler
func NewRateLimitScheduler(cfg *config.Config, redisClient store.RedisClientIface) *RateLimitScheduler {
	return &RateLimitScheduler{
		config:           cfg,
		redisClient:      redisClient,
		activeMonitoring: make(map[string]bool),
	}
}

// StartMonitoring starts monitoring an endpoint for rate limit recovery
func (rls *RateLimitScheduler) StartMonitoring(chain, endpointID string) {
	key := chain + ":" + endpointID

	rls.mu.Lock()
	// Don't start monitoring if already active
	if rls.activeMonitoring[key] {
		rls.mu.Unlock()
		log.Debug().Str("chain", chain).Str("endpoint", endpointID).Msg("Rate limit monitoring already active")
		return
	}
	rls.activeMonitoring[key] = true
	rls.mu.Unlock()

	log.Info().Str("chain", chain).Str("endpoint", endpointID).Msg("Starting rate limit recovery monitoring")

	// Start monitoring in a goroutine
	go rls.monitorEndpoint(chain, endpointID)
}

// monitorEndpoint performs periodic recovery checks for a rate-limited endpoint
func (rls *RateLimitScheduler) monitorEndpoint(chain, endpointID string) {
	key := chain + ":" + endpointID

	// Clean up monitoring flag when done
	defer func() {
		rls.mu.Lock()
		delete(rls.activeMonitoring, key)
		rls.mu.Unlock()
		log.Debug().Str("chain", chain).Str("endpoint", endpointID).Msg("Rate limit monitoring stopped")
	}()

	// Get endpoint configuration
	chainEndpoints, exists := rls.config.GetEndpointsForChain(chain)
	if !exists {
		log.Error().Str("chain", chain).Msg("Chain not found in configuration")
		return
	}

	endpoint, exists := chainEndpoints[endpointID]
	if !exists {
		log.Error().Str("chain", chain).Str("endpoint", endpointID).Msg("Endpoint not found in configuration")
		return
	}

	// Get rate limit recovery configuration
	rateLimitConfig := config.DefaultRateLimitRecovery()
	if endpoint.RateLimitRecovery != nil {
		rateLimitConfig = *endpoint.RateLimitRecovery
	}

	// Use dynamic backoff instead of fixed ticker
	for {
		// Get current state to determine next check time
		state, err := rls.redisClient.GetRateLimitState(context.Background(), chain, endpointID)
		if err != nil {
			log.Error().Err(err).Str("chain", chain).Str("endpoint", endpointID).Msg("Failed to get rate limit state for scheduling")
			return
		}

		// Check if we should reset the backoff cycle
		if rls.shouldResetBackoff(state, rateLimitConfig) {
			log.Info().Str("chain", chain).Str("endpoint", endpointID).Msg("Resetting rate limit backoff cycle")
			state.RecoveryAttempts = 0
			state.CurrentBackoff = 0
			state.ConsecutiveSuccess = 0
			state.FirstRateLimited = time.Now()
			if err := rls.redisClient.SetRateLimitState(context.Background(), chain, endpointID, *state); err != nil {
				log.Error().Err(err).Str("chain", chain).Str("endpoint", endpointID).Msg("Failed to reset rate limit state")
				return
			}
		}

		// Calculate next backoff time
		nextBackoff := rls.calculateNextBackoff(state, rateLimitConfig)

		log.Debug().
			Str("chain", chain).
			Str("endpoint", endpointID).
			Int("next_backoff", nextBackoff).
			Int("attempt", state.RecoveryAttempts).
			Msg("Scheduling next rate limit recovery check")

		// Wait for the calculated backoff time
		time.Sleep(time.Duration(nextBackoff) * time.Second)

		// Check if we should continue monitoring
		shouldContinue, err := rls.performRecoveryCheck(chain, endpointID, endpoint, rateLimitConfig)
		if err != nil {
			log.Error().Err(err).Str("chain", chain).Str("endpoint", endpointID).Msg("Error during recovery check")
			continue
		}
		if !shouldContinue {
			return // Stop monitoring
		}
	}
}

// performRecoveryCheck performs a single recovery check for an endpoint
func (rls *RateLimitScheduler) performRecoveryCheck(chain, endpointID string, endpoint config.Endpoint, rateLimitConfig config.RateLimitRecovery) (bool, error) {
	// Get current rate limit state
	state, err := rls.redisClient.GetRateLimitState(context.Background(), chain, endpointID)
	if err != nil {
		return false, err
	}

	// If no longer rate limited, stop monitoring
	if !state.RateLimited {
		log.Debug().Str("chain", chain).Str("endpoint", endpointID).Msg("Endpoint no longer rate limited, stopping monitoring")
		return false, nil
	}

	// Check if we've exceeded max retries
	if state.RecoveryAttempts >= rateLimitConfig.MaxRetries {
		log.Warn().
			Str("chain", chain).
			Str("endpoint", endpointID).
			Int("attempts", state.RecoveryAttempts).
			Int("max_retries", rateLimitConfig.MaxRetries).
			Msg("Rate limit recovery max retries exceeded, stopping monitoring")
		return false, nil
	}

	log.Debug().Str("chain", chain).Str("endpoint", endpointID).Int("attempt", state.RecoveryAttempts+1).Msg("Performing rate limit recovery check")

	// Increment recovery attempts and update backoff
	state.RecoveryAttempts++
	state.LastRecoveryCheck = time.Now()

	// Update current backoff for next iteration
	if state.CurrentBackoff == 0 {
		state.CurrentBackoff = rateLimitConfig.InitialBackoff
	} else {
		newBackoff := int(float64(state.CurrentBackoff) * rateLimitConfig.BackoffMultiplier)
		state.CurrentBackoff = min(newBackoff, rateLimitConfig.MaxBackoff)
	}

	// Perform the health check
	success := rls.checkEndpointHealth(endpoint)

	if success {
		state.ConsecutiveSuccess++
		if state.ConsecutiveSuccess == 1 {
			// Reset backoff to initial value after the first successful check
			state.CurrentBackoff = rateLimitConfig.InitialBackoff
		}
		log.Debug().
			Str("chain", chain).
			Str("endpoint", endpointID).
			Int("consecutive_success", state.ConsecutiveSuccess).
			Int("required", rateLimitConfig.RequiredSuccesses).
			Msg("Rate limit recovery check succeeded")

		// Check if we have enough consecutive successes
		if state.ConsecutiveSuccess >= rateLimitConfig.RequiredSuccesses {
			// Mark as recovered
			state.RateLimited = false
			state.RecoveryAttempts = 0
			state.ConsecutiveSuccess = 0
			state.CurrentBackoff = 0
			state.FirstRateLimited = time.Time{} // Clear the first rate limited time

			// Update endpoint status to healthy
			endpointStatus, err := rls.redisClient.GetEndpointStatus(context.Background(), chain, endpointID)
			if err != nil {
				log.Error().Err(err).Str("chain", chain).Str("endpoint", endpointID).Msg("Failed to get endpoint status")
			} else {
				if endpoint.HTTPURL != "" {
					endpointStatus.HealthyHTTP = true
				}
				if endpoint.WSURL != "" {
					endpointStatus.HealthyWS = true
				}

				if err := rls.redisClient.UpdateEndpointStatus(context.Background(), chain, endpointID, *endpointStatus); err != nil {
					log.Error().Err(err).Str("chain", chain).Str("endpoint", endpointID).Msg("Failed to update endpoint status")
				} else {
					log.Info().
						Str("chain", chain).
						Str("endpoint", endpointID).
						Int("successful_checks", rateLimitConfig.RequiredSuccesses).
						Msg("Endpoint recovered from rate limiting")
				}
			}

			// Save state and stop monitoring
			if err := rls.redisClient.SetRateLimitState(context.Background(), chain, endpointID, *state); err != nil {
				log.Error().Err(err).Str("chain", chain).Str("endpoint", endpointID).Msg("Failed to save recovery state")
			}
			return false, nil // Stop monitoring
		}
	} else {
		// Reset consecutive success count on failure
		state.ConsecutiveSuccess = 0
		log.Debug().Str("chain", chain).Str("endpoint", endpointID).Msg("Rate limit recovery check failed, resetting consecutive success count")
	}

	// Save updated state
	if err := rls.redisClient.SetRateLimitState(context.Background(), chain, endpointID, *state); err != nil {
		log.Error().Err(err).Str("chain", chain).Str("endpoint", endpointID).Msg("Failed to save rate limit state")
		return false, err
	}

	return true, nil // Continue monitoring
}

// checkEndpointHealth performs a simple HTTP health check on the endpoint
func (rls *RateLimitScheduler) checkEndpointHealth(endpoint config.Endpoint) bool {
	if endpoint.HTTPURL == "" {
		return false
	}

	// Create a simple HTTP client with timeout
	client := &http.Client{
		Timeout: 10 * time.Second,
	}

	// Create a simple POST request (similar to what the proxy would do)
	req, err := http.NewRequest("POST", endpoint.HTTPURL, http.NoBody)
	if err != nil {
		log.Debug().Err(err).Str("url", helpers.RedactAPIKey(endpoint.HTTPURL)).Msg("Failed to create recovery check request")
		return false
	}

	// Set appropriate headers
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		log.Debug().Err(err).Str("url", helpers.RedactAPIKey(endpoint.HTTPURL)).Msg("Recovery check request failed")
		return false
	}
	defer resp.Body.Close()

	// Consider 2xx responses as success, 429 as still rate limited, others as failure
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		log.Debug().Str("url", helpers.RedactAPIKey(endpoint.HTTPURL)).Int("status", resp.StatusCode).Msg("Recovery check successful")
		return true
	}

	if resp.StatusCode == 429 {
		log.Debug().Str("url", helpers.RedactAPIKey(endpoint.HTTPURL)).Msg("Recovery check still rate limited")
	} else {
		log.Debug().Str("url", helpers.RedactAPIKey(endpoint.HTTPURL)).Int("status", resp.StatusCode).Msg("Recovery check failed with error status")
	}

	return false
}

// shouldResetBackoff determines if the backoff cycle should be reset
func (rls *RateLimitScheduler) shouldResetBackoff(state *store.RateLimitState, config config.RateLimitRecovery) bool {
	if state.FirstRateLimited.IsZero() {
		return false
	}

	timeSinceFirst := time.Since(state.FirstRateLimited)
	resetDuration := time.Duration(config.ResetAfter) * time.Second

	return timeSinceFirst >= resetDuration
}

// calculateNextBackoff calculates the next backoff time based on current state
func (rls *RateLimitScheduler) calculateNextBackoff(state *store.RateLimitState, config config.RateLimitRecovery) int {
	if state.CurrentBackoff == 0 {
		return config.InitialBackoff
	}
	return state.CurrentBackoff
}
