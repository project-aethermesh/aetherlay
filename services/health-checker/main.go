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

// main initializes and starts the health checker service
func main() {
	// Load .env file if present
	_ = godotenv.Load()

	// Initialize logger
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stdout})

	// Set zerolog global log level from ZEROLOG_LEVEL env var, default to "info"
	levelStr := helpers.GetStringFromEnv("ZEROLOG_LEVEL", "info")
	if level, err := zerolog.ParseLevel(levelStr); err == nil {
		zerolog.SetGlobalLevel(level)
	} else {
		log.Warn().Str("ZEROLOG_LEVEL", levelStr).Msg("Invalid ZEROLOG_LEVEL, defaulting to Info")
		zerolog.SetGlobalLevel(zerolog.InfoLevel)
	}

	// Parse command line flags
	configPath := flag.String("config", "configs/endpoints.json", "Path to the endpoints configuration file")
	healthCheckInterval := flag.Int("health-check-interval", helpers.GetIntFromEnv("HEALTH_CHECK_INTERVAL", 30), "Health check interval in seconds (overrides HEALTH_CHECK_INTERVAL env var)")
	redisAddr := flag.String("redis-addr", helpers.GetStringFromEnv("REDIS_HOST", "localhost") + ":" + helpers.GetStringFromEnv("REDIS_PORT", "6379"), "Redis server address")
	standaloneHealthChecks := flag.Bool("standalone-health-checks", helpers.GetBoolFromEnv("STANDALONE_HEALTH_CHECKS", true), "Enable standalone health checks (overrides STANDALONE_HEALTH_CHECKS env var)")
	flag.Parse()

	// Get Redis password from the env var
	redisPassword := helpers.GetStringFromEnv("REDIS_PASS", "")

	// If HEALTH_CHECK_INTERVAL == 0, do not run the standalone health checker, only ephemeral checks should run
	if *healthCheckInterval == 0 {
		log.Warn().Msg("HEALTH_CHECK_INTERVAL=0 detected. Standalone health checker will not run, only ephemeral checks will run when needed.")
		return
	}

	// Check if standalone health checks are enabled
	if !*standaloneHealthChecks {
		log.Warn().Msg("Standalone health checks disabled (STANDALONE_HEALTH_CHECKS=false). Exiting.")
		return
	}

	// Load configuration
	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to load configuration")
	}

	// Print the loaded configuration
	log.Info().Msg("Health Checker Service - Loaded configuration:")
	for chainName, chainEndpoints := range cfg.Endpoints {
		log.Info().Str("chain", chainName).Msg("Chain configuration")
		for endpointName, endpoint := range chainEndpoints {
			log.Info().
				Str("chain", chainName).
				Str("endpoint", endpointName).
				Str("provider", endpoint.Provider).
				Str("role", endpoint.Role).
				Str("type", endpoint.Type).
				Str("rpc_url", helpers.RedactAPIKey(endpoint.RPCURL)).
				Str("ws_url", helpers.RedactAPIKey(endpoint.WSURL)).
				Msg("Endpoint configuration")
		}
	}

	// Initialize Redis client
	redisClient := store.NewRedisClient(*redisAddr, redisPassword)
	if err := redisClient.Ping(context.Background()); err != nil {
		log.Fatal().Err(err).Msg("Failed to connect to Redis")
	}
	defer redisClient.Close()

	// Configure health check
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	checker := health.NewChecker(cfg, redisClient, time.Duration(*healthCheckInterval)*time.Second)

	if *healthCheckInterval == 0 {
		// If the health check service was disabled, run the check once and exit
		log.Info().Msg("Health checks disabled (HEALTH_CHECK_INTERVAL=0), running initial health check only")
		checker.RunOnce(ctx)
		return
	}

	// Run the health check as a background service
	log.Info().Int("interval_seconds", *healthCheckInterval).Msg("Starting standalone health check service")
	go checker.Start(ctx)

	// Handle graceful shutdown
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	<-stop
	log.Info().Msg("Shutting down health checker service...")
}
