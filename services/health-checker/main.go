package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"aetherlay/internal/config"
	"aetherlay/internal/health"
	"aetherlay/internal/helpers"
	"aetherlay/internal/metrics"
	"aetherlay/internal/store"

	"github.com/joho/godotenv"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// onModeDetected is a test hook for registering the detected mode. It is set by tests only.
var onModeDetected func(string)

// Allow patching in tests
var newValkeyClient func(addr string, password string, skipTLSVerify bool, valkeyUseTLS bool) store.ValkeyClientIface = func(addr string, password string, skipTLSVerify bool, valkeyUseTLS bool) store.ValkeyClientIface {
	return store.NewValkeyClient(addr, password, skipTLSVerify, valkeyUseTLS)
}
var loadConfig = config.LoadConfig

// testCheckerPatch is a test hook for patching the Checker instance in tests
var testCheckerPatch func(*health.Checker)

// testExitAfterSetup is a test hook to exit main after setup in tests
var testExitAfterSetup bool

// exitCode is used to track the exit code for the process
var exitCode int

// standaloneInitialBackoff mirrors server.Server.initialBackoffForSignal: seed the first
// recovery-check wait from whatever the provider actually told us, instead of always
// guessing. A parsed Retry-After is used directly; Infura's daily credit cap (402) seeds
// from the endpoint's own (or default) MaxBackoff, since the docs don't guarantee an
// exact reset boundary. Otherwise 0, leaving the load balancer's scheduler to fall back
// to InitialBackoff once it picks up recovery monitoring.
func standaloneInitialBackoff(cfg *config.Config, chain, endpointID string, signal health.RateLimitSignal) int {
	if signal.RetryAfter > 0 {
		return int(signal.RetryAfter.Seconds())
	}
	if signal.IsDailyQuota {
		rlc := config.DefaultRateLimitRecovery()
		if chainEndpoints, ok := cfg.GetEndpointsForChain(chain); ok {
			if ep, ok := chainEndpoints[endpointID]; ok && ep.RateLimitRecovery != nil && ep.RateLimitRecovery.MaxBackoff != 0 {
				rlc.MaxBackoff = ep.RateLimitRecovery.MaxBackoff
			}
		}
		return rlc.MaxBackoff
	}
	return 0
}

// applyStandaloneLearnedCapacityDecrease mirrors server.Server.applyLearnedCapacityDecrease
// exactly, using the same shared store.ApplyCapacityDecrease/EffectiveMaxRequests math, so
// the load balancer and the standalone health checker - two separate processes mutating
// the same Valkey-persisted estimate - never diverge. Only engages for endpoints with no
// static Capacity configured, mirroring effectiveCapacityCeiling's rule in server.go.
func applyStandaloneLearnedCapacityDecrease(cfg *config.Config, valkeyClient store.ValkeyClientIface, capacityThrottlingEnabled, capacityLearningEnabled bool, chain, endpointID string) {
	chainEndpoints, ok := cfg.GetEndpointsForChain(chain)
	if !ok {
		return
	}
	ep, ok := chainEndpoints[endpointID]
	if !ok {
		return
	}
	if !capacityThrottlingEnabled || !capacityLearningEnabled || ep.Capacity != nil {
		return
	}

	params := config.ResolveCapacityLearning(ep.CapacityLearning)
	now := time.Now()
	ctx := context.Background()

	prior, err := valkeyClient.GetCapacityEstimate(ctx, chain, endpointID)
	if err != nil {
		log.Debug().Err(err).Str("chain", chain).Str("endpoint", endpointID).Msg("Failed to get capacity estimate in standalone health checker")
		return
	}

	// Read against the estimate's own frozen window once one exists (matching
	// server.Server.capacityWindowSeconds/effectiveCapacityCeiling exactly), not the
	// live-resolved config - otherwise this read targets a different Valkey bucket key
	// than the load balancer's writes. Only before any estimate has been seeded is there
	// no frozen value to match.
	readWindowSeconds := params.WindowSeconds
	if prior.HasEstimate {
		readWindowSeconds = prior.WindowSeconds
	}

	if !store.ShouldApplyCapacityDecrease(*prior, readWindowSeconds, now) {
		log.Debug().Str("chain", chain).Str("endpoint", endpointID).Msg("Skipping capacity estimate decrease in standalone health checker, within cooldown")
		return
	}

	observedCount, err := valkeyClient.GetCapacityCount(ctx, chain, endpointID, readWindowSeconds)
	if err != nil {
		log.Debug().Err(err).Str("chain", chain).Str("endpoint", endpointID).Msg("Failed to get capacity count in standalone health checker")
		return
	}

	effectiveNow := store.EffectiveMaxRequests(*prior, params, now)
	newEstimate := store.ApplyCapacityDecrease(*prior, effectiveNow, observedCount, params.WindowSeconds, params, now)

	if err := valkeyClient.SetCapacityEstimate(ctx, chain, endpointID, newEstimate); err != nil {
		log.Debug().Err(err).Str("chain", chain).Str("endpoint", endpointID).Msg("Failed to set capacity estimate in standalone health checker")
		return
	}

	log.Info().Str("chain", chain).Str("endpoint", endpointID).Int64("new_estimate", newEstimate.MaxRequests).Msg("Standalone health checker decreased learned capacity estimate after a rate-limit hit")
	if metrics.EndpointCapacityEstimatedCeiling != nil {
		metrics.EndpointCapacityEstimatedCeiling.WithLabelValues(chain, endpointID).Set(float64(newEstimate.MaxRequests))
	}
	if metrics.EndpointCapacityEstimateDecreasedTotal != nil {
		metrics.EndpointCapacityEstimateDecreasedTotal.WithLabelValues(chain, endpointID).Inc()
	}
}

// createStandaloneRateLimitHandler creates a simple rate limit handler for the standalone health checker
func createStandaloneRateLimitHandler(cfg *config.Config, valkeyClient store.ValkeyClientIface, capacityThrottlingEnabled, capacityLearningEnabled bool) func(chain, endpointID, protocol string, signal health.RateLimitSignal) {
	return func(chain, endpointID, protocol string, signal health.RateLimitSignal) {
		log.Debug().Str("chain", chain).Str("endpoint", endpointID).Str("protocol", protocol).Msg("Standalone health checker detected rate limit")

		// Get current rate limit state
		state, err := valkeyClient.GetRateLimitState(context.Background(), chain, endpointID)
		if err != nil {
			log.Error().Err(err).Str("chain", chain).Str("endpoint", endpointID).Msg("Failed to get rate limit state in standalone health checker")
			return
		}

		// Mark as rate limited but don't start recovery scheduling (that's for the main load balancer)
		now := time.Now()
		state.RateLimited = true
		state.LastRecoveryCheck = now
		state.CurrentBackoff = standaloneInitialBackoff(cfg, chain, endpointID, signal)

		// Set first rate limited time if this is the first time
		if state.FirstRateLimited.IsZero() {
			state.FirstRateLimited = now
		}

		if err := valkeyClient.SetRateLimitState(context.Background(), chain, endpointID, *state); err != nil {
			log.Error().Err(err).Str("chain", chain).Str("endpoint", endpointID).Msg("Failed to set rate limit state in standalone health checker")
			return
		}

		log.Info().Str("chain", chain).Str("endpoint", endpointID).Str("protocol", protocol).Int("current_backoff", state.CurrentBackoff).Msg("Standalone health checker marked endpoint as rate limited")

		// Check for rate limits first (this signal), then approximate the endpoint's
		// safe throughput ceiling from it - only engages for endpoints with no static
		// Capacity, and shares the exact same math as the load balancer's own path.
		applyStandaloneLearnedCapacityDecrease(cfg, valkeyClient, capacityThrottlingEnabled, capacityLearningEnabled, chain, endpointID)
	}
}

// RunHealthChecker runs the health checker service with the given configuration.
func RunHealthChecker(
	configFile string,
	corsHeaders string,
	corsMethods string,
	corsOrigin string,
	capacityLearningEnabled bool,
	capacityThrottlingEnabled bool,
	ephemeralChecksEnabled bool,
	ephemeralChecksHealthyThreshold int,
	ephemeralChecksInterval int,
	healthCheckConcurrency int,
	healthCheckInterval int,
	healthCheckSyncStatus bool,
	healthCheckerServerPort int,
	metricsEnabled bool,
	metricsPort int,
	valkeyHost string,
	valkeyPass string,
	valkeyPort string,
	valkeySkipTLSCheck bool,
	valkeyUseTLS bool,
	standaloneHealthChecks bool,
) {

	mode := ""

	if !standaloneHealthChecks {
		mode = "disabled"
		if onModeDetected != nil {
			onModeDetected(mode)
		}
		if testExitAfterSetup {
			return
		}
		log.Warn().Msg("Standalone health checks disabled (STANDALONE_HEALTH_CHECKS=false). Exiting.")
		return
	}

	// Start HTTP server FIRST, before any dependencies (config, Valkey)
	// This ensures health probes are able to be used from the start
	log.Info().Int("port", healthCheckerServerPort).Msg("Starting HTTP server before dependencies")
	httpServer := health.NewHealthCheckerServer(healthCheckerServerPort, nil) // Start with nil checker
	startupErrCh := make(chan error, 1)
	httpServer.Start(startupErrCh)

	// Wait for startup result from the HTTP server goroutine (bind errors are immediate)
	if err := <-startupErrCh; err != nil {
		log.Error().Err(err).Msg("Health checker HTTP server failed to start")
		exitCode = 1
		return
	}
	log.Info().Msg("HTTP server startup successful, proceeding with dependency initialization")

	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			log.Error().Err(err).Msg("Error shutting down health checker HTTP server")
		}
	}()

	// Start the metrics server if enabled
	if metricsEnabled {
		log.Info().Int("port", metricsPort).Msg("Prometheus metrics server enabled")
		metrics.StartServer(metricsPort, corsHeaders, corsMethods, corsOrigin)
	}

	cfg, err := loadConfig(configFile)
	if err != nil {
		log.Error().Err(err).Msg("Failed to load configuration")
		exitCode = 1
		return
	}

	log.Info().Msg("Health Checker Service - Loaded configuration:")
	for chainName, chainEndpoints := range cfg.Endpoints {
		log.Info().Str("chain", chainName).Msg("Chain configuration")
		for endpointID, endpoint := range chainEndpoints {
			log.Info().
				Str("chain", chainName).
				Str("endpoint", endpointID).
				Str("provider", endpoint.Provider).
				Str("role", endpoint.Role).
				Str("type", endpoint.Type).
				Str("http_url", helpers.RedactAPIKey(endpoint.HTTPURL)).
				Str("ws_url", helpers.RedactAPIKey(endpoint.WSURL)).
				Msg("Endpoint configuration")
		}
	}

	valkeyAddr := valkeyHost + ":" + valkeyPort
	valkeyClient := newValkeyClient(valkeyAddr, valkeyPass, valkeySkipTLSCheck, valkeyUseTLS)
	if err := valkeyClient.Ping(context.Background()); err != nil {
		log.Error().Err(err).Msg("Failed to connect to Valkey")
		exitCode = 1
		return
	}
	defer valkeyClient.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	checker := health.NewChecker(cfg, valkeyClient, time.Duration(healthCheckInterval)*time.Second, time.Duration(ephemeralChecksInterval)*time.Second, ephemeralChecksHealthyThreshold, healthCheckSyncStatus, healthCheckConcurrency, ephemeralChecksEnabled)

	// Set up simple rate limit handler for standalone health checker
	checker.HandleRateLimitFunc = createStandaloneRateLimitHandler(cfg, valkeyClient, capacityThrottlingEnabled, capacityLearningEnabled)

	if testCheckerPatch != nil {
		testCheckerPatch(checker)
	}

	// Update HTTP server with checker instance now that dependencies are loaded
	log.Info().Msg("Dependencies loaded, updating HTTP server with checker instance")
	httpServer.SetChecker(checker)

	if healthCheckInterval == 0 {
		mode = "ephemeral"
		if onModeDetected != nil {
			onModeDetected(mode)
		}
		if testExitAfterSetup {
			return
		}
		log.Info().Msg("HEALTH_CHECK_INTERVAL=0, only ephemeral checks will run when needed.")
		go checker.StartEphemeralChecks(ctx)
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
		<-stop
		log.Info().Msg("Shutting down health checker service for ephemeral checks...")
		return
	}

	mode = "standalone"
	if onModeDetected != nil {
		onModeDetected(mode)
	}
	if testExitAfterSetup {
		return
	}
	log.Info().Int("interval_seconds", healthCheckInterval).Msg("Starting standalone health check service")
	go checker.Start(ctx)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Info().Msg("Shutting down health checker service...")
}

// main initializes and starts the health checker service
func main() {
	_ = godotenv.Load()

	// Initialize logger
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stdout})

	// Parse CLI flags and load configuration
	flagConfig := helpers.ParseFlags()
	config := flagConfig.LoadConfiguration()

	// Set the requested log level if it's valid, otherwise default to info
	if level, err := zerolog.ParseLevel(config.LogLevel); err == nil {
		zerolog.SetGlobalLevel(level)
	} else {
		log.Warn().Str("LOG_LEVEL", config.LogLevel).Msg("Invalid log level, defaulting to Info")
		zerolog.SetGlobalLevel(zerolog.InfoLevel)
	}

	RunHealthChecker(
		config.ConfigFile,
		config.CorsHeaders,
		config.CorsMethods,
		config.CorsOrigin,
		config.CapacityLearningEnabled,
		config.CapacityThrottlingEnabled,
		config.EphemeralChecksEnabled,
		config.EphemeralChecksHealthyThreshold,
		config.EphemeralChecksInterval,
		config.HealthCheckConcurrency,
		config.HealthCheckInterval,
		config.HealthCheckSyncStatus,
		config.HealthCheckerServerPort,
		config.MetricsEnabled,
		config.MetricsPort,
		config.ValkeyHost,
		config.ValkeyPass,
		config.ValkeyPort,
		config.ValkeySkipTLSCheck,
		config.ValkeyUseTLS,
		config.StandaloneHealthChecks,
	)

	// Exit with the appropriate code after RunHealthChecker returns
	// This allows defers in RunHealthChecker to run, while not hiding panics
	os.Exit(exitCode)
}
