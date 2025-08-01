package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	"aetherlay/internal/config"
	"aetherlay/internal/health"
	"aetherlay/internal/helpers"
	"aetherlay/internal/store"

	"github.com/joho/godotenv"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// onModeDetected is a test hook for registering the detected mode. It is set by tests only.
var onModeDetected func(string)

// Allow patching in tests
var newRedisClient func(addr string, password string) store.RedisClientIface = func(addr string, password string) store.RedisClientIface {
	return store.NewRedisClient(addr, password)
}
var loadConfig = config.LoadConfig

// testCheckerPatch is a test hook for patching the Checker instance in tests
var testCheckerPatch func(*health.Checker)

// testExitAfterSetup is a test hook to exit main after setup in tests
var testExitAfterSetup bool

// RunHealthChecker runs the health checker service with the given configuration.
func RunHealthChecker(
	configFile string,
	ephemeralChecksInterval int,
	ephemeralChecksHealthyThreshold int,
	healthCheckInterval int,
	redisHost string,
	redisPort string,
	redisPassword string,
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
	redisClient := newRedisClient(redisAddr, redisPassword)
	if err := redisClient.Ping(context.Background()); err != nil {
		log.Fatal().Err(err).Msg("Failed to connect to Redis")
	}
	defer redisClient.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	checker := health.NewChecker(cfg, redisClient, time.Duration(healthCheckInterval)*time.Second, time.Duration(ephemeralChecksInterval)*time.Second, ephemeralChecksHealthyThreshold)
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
		log.Info().Msg("HEALTH_CHECK_INTERVAL=0: Only ephemeral checks will run when needed.")
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

	// Parse command line flags
	configFile := flag.String(
		"config-file",
		helpers.GetStringFromEnv("CONFIG_FILE", "configs/endpoints.json"),
		"Path to the endpoints configuration file",
	)
	ephemeralChecksInterval := flag.Int(
		"ephemeral-checks-interval",
		helpers.GetIntFromEnv("EPHEMERAL_CHECKS_INTERVAL", 30),
		"Interval in seconds for ephemeral health checks",
	)
	ephemeralChecksHealthyThreshold := flag.Int(
		"ephemeral-checks-healthy-threshold",
		helpers.GetIntFromEnv("EPHEMERAL_CHECKS_HEALTHY_THRESHOLD", 3),
		"Amount of consecutive successful responses required to consider endpoint healthy again",
	)
	healthCheckInterval := flag.Int(
		"health-check-interval",
		helpers.GetIntFromEnv("HEALTH_CHECK_INTERVAL", 30),
		"Health check interval in seconds",
	)
	logLevel := flag.String(
		"log-level",
		helpers.GetStringFromEnv("LOG_LEVEL", "info"),
		"Set the log level",
	)
	redisHost := flag.String(
		"redis-host",
		helpers.GetStringFromEnv("REDIS_HOST", "localhost"),
		"Redis server hostname",
	)
	redisPort := flag.String(
		"redis-port",
		helpers.GetStringFromEnv("REDIS_PORT", "6379"),
		"Redis server port",
	)
	standaloneHealthChecks := flag.Bool(
		"standalone-health-checks",
		helpers.GetBoolFromEnv("STANDALONE_HEALTH_CHECKS", true),
		"Enable standalone health checks",
	)
	flag.Parse()
	redisPassword := helpers.GetStringFromEnv("REDIS_PASS", "")

	// Set the requested log level if it's valid, otherwise default to info
	if level, err := zerolog.ParseLevel(*logLevel); err == nil {
		zerolog.SetGlobalLevel(level)
	} else {
		log.Warn().Str("LOG_LEVEL", *logLevel).Msg("Invalid log level, defaulting to Info")
		zerolog.SetGlobalLevel(zerolog.InfoLevel)
	}

	RunHealthChecker(
		*configFile,
		*ephemeralChecksInterval,
		*ephemeralChecksHealthyThreshold,
		*healthCheckInterval,
		*redisHost,
		*redisPort,
		redisPassword,
		*standaloneHealthChecks,
	)
}
