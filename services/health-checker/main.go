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
var newRedisClient func(addr string, password string, skipTLSVerify bool, redisUseTLS bool) store.RedisClientIface = func(addr string, password string, skipTLSVerify bool, redisUseTLS bool) store.RedisClientIface {
	return store.NewRedisClient(addr, password, skipTLSVerify, redisUseTLS)
}
var loadConfig = config.LoadConfig

// testCheckerPatch is a test hook for patching the Checker instance in tests
var testCheckerPatch func(*health.Checker)

// testExitAfterSetup is a test hook to exit main after setup in tests
var testExitAfterSetup bool

// createStandaloneRateLimitHandler creates a simple rate limit handler for the standalone health checker
func createStandaloneRateLimitHandler(redisClient store.RedisClientIface) func(chain, endpointID, protocol string) {
	return func(chain, endpointID, protocol string) {
		log.Debug().Str("chain", chain).Str("endpoint", endpointID).Str("protocol", protocol).Msg("Standalone health checker detected rate limit")

		// Get current rate limit state
		state, err := redisClient.GetRateLimitState(context.Background(), chain, endpointID)
		if err != nil {
			log.Error().Err(err).Str("chain", chain).Str("endpoint", endpointID).Msg("Failed to get rate limit state in standalone health checker")
			return
		}

		// Mark as rate limited but don't start recovery scheduling (that's for the main load balancer)
		now := time.Now()
		state.RateLimited = true
		state.LastRecoveryCheck = now

		// Set first rate limited time if this is the first time
		if state.FirstRateLimited.IsZero() {
			state.FirstRateLimited = now
		}

		if err := redisClient.SetRateLimitState(context.Background(), chain, endpointID, *state); err != nil {
			log.Error().Err(err).Str("chain", chain).Str("endpoint", endpointID).Msg("Failed to set rate limit state in standalone health checker")
			return
		}

		log.Info().Str("chain", chain).Str("endpoint", endpointID).Str("protocol", protocol).Msg("Standalone health checker marked endpoint as rate limited")
	}
}

// RunHealthChecker runs the health checker service with the given configuration.
func RunHealthChecker(
	configFile string,
	corsHeaders string,
	corsMethods string,
	corsOrigin string,
	ephemeralChecksHealthyThreshold int,
	ephemeralChecksInterval int,
	healthCheckInterval int,
	healthCheckSyncStatus bool,
	metricsEnabled bool,
	metricsPort int,
	redisHost string,
	redisPass string,
	redisPort string,
	redisSkipTLSCheck bool,
	redisUseTLS bool,
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

	// Start the metrics server if enabled
	if metricsEnabled {
		log.Info().Int("port", metricsPort).Msg("Prometheus metrics server enabled")
		metrics.StartServer(metricsPort, corsHeaders, corsMethods, corsOrigin)
	}

	cfg, err := loadConfig(configFile)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to load configuration")
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

	redisAddr := redisHost + ":" + redisPort
	redisClient := newRedisClient(redisAddr, redisPass, redisSkipTLSCheck, redisUseTLS)
	if err := redisClient.Ping(context.Background()); err != nil {
		log.Fatal().Err(err).Msg("Failed to connect to Redis")
	}
	defer redisClient.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	checker := health.NewChecker(cfg, redisClient, time.Duration(healthCheckInterval)*time.Second, time.Duration(ephemeralChecksInterval)*time.Second, ephemeralChecksHealthyThreshold, healthCheckSyncStatus)

	// Set up simple rate limit handler for standalone health checker
	checker.HandleRateLimitFunc = createStandaloneRateLimitHandler(redisClient)

	if testCheckerPatch != nil {
		testCheckerPatch(checker)
	}

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
		config.EphemeralChecksHealthyThreshold,
		config.EphemeralChecksInterval,
		config.HealthCheckInterval,
		config.HealthCheckSyncStatus,
		config.MetricsEnabled,
		config.MetricsPort,
		config.RedisHost,
		config.RedisPass,
		config.RedisPort,
		config.RedisSkipTLSCheck,
		config.RedisUseTLS,
		config.StandaloneHealthChecks,
	)
}
