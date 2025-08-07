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
	"aetherlay/internal/metrics"
	"aetherlay/internal/server"
	"aetherlay/internal/store"

	"github.com/joho/godotenv"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// main initializes and starts the RPC load balancer service
func main() {
	// Load .env file if present
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
	logLevel := flag.String(
		"log-level",
		helpers.GetStringFromEnv("LOG_LEVEL", "info"),
		"Set the log level",
	)
	metricsEnabled := flag.Bool(
		"metrics-enabled",
		helpers.GetBoolFromEnv("METRICS_ENABLED", true),
		"Enable the Prometheus metrics server",
	)
	metricsPort := flag.Int(
		"metrics-port",
		helpers.GetIntFromEnv("METRICS_PORT", 9091),
		"Port for the Prometheus metrics server",
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
	serverPort := flag.Int(
		"server-port",
		helpers.GetIntFromEnv("SERVER_PORT", 8080),
		"Server port",
	)
	redisUseTLS := flag.Bool(
		"redis-use-tls",
		helpers.GetBoolFromEnv("REDIS_USE_TLS", false),
		"Use TLS for Redis connection",
	)
	standaloneHealthChecks := flag.Bool(
		"standalone-health-checks",
		helpers.GetBoolFromEnv("STANDALONE_HEALTH_CHECKS", true),
		"Enable standalone health checks",
	)

	// These flags are only used if the standalone health checker is NOT enabled
	var ephemeralChecksInterval, ephemeralChecksHealthyThreshold, healthCheckInterval *int
	if !*standaloneHealthChecks {
		ephemeralChecksInterval = flag.Int(
			"ephemeral-checks-interval",
			helpers.GetIntFromEnv("EPHEMERAL_CHECKS_INTERVAL", 30),
			"Interval in seconds for ephemeral health checks",
		)
		ephemeralChecksHealthyThreshold = flag.Int(
			"ephemeral-checks-healthy-threshold",
			helpers.GetIntFromEnv("EPHEMERAL_CHECKS_HEALTHY_THRESHOLD", 3),
			"Amount of consecutive successful responses required to consider endpoint healthy again",
		)
		healthCheckInterval = flag.Int(
			"health-check-interval",
			helpers.GetIntFromEnv("HEALTH_CHECK_INTERVAL", 30),
			"Health check interval in seconds",
		)
	}

	flag.Parse()

	// Get Redis password from the env var
	redisPassword := helpers.GetStringFromEnv("REDIS_PASS", "")

	// Set the requested log level if it's valid, otherwise default to info
	if level, err := zerolog.ParseLevel(*logLevel); err == nil {
		zerolog.SetGlobalLevel(level)
	} else {
		log.Warn().Str("LOG_LEVEL", *logLevel).Msg("Invalid log level, defaulting to Info")
		zerolog.SetGlobalLevel(zerolog.InfoLevel)
	}

	// Start the metrics server if enabled
	if *metricsEnabled {
		log.Info().Int("port", *metricsPort).Msg("Prometheus metrics server enabled")
		metrics.StartServer(*metricsPort)
	}

	// Load configuration
	cfg, err := config.LoadConfig(*configFile)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to load configuration")
	}

	// Print the loaded configuration to see the substitutions
	log.Info().Msg("RPC Load Balancer - Loaded configuration:")
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

	// Initialize Redis client
	redisAddr := *redisHost + ":" + *redisPort
	redisClient := store.NewRedisClient(redisAddr, redisPassword, *redisUseTLS)
	if err := redisClient.Ping(context.Background()); err != nil {
		log.Fatal().Err(err).Msg("Failed to connect to Redis")
	}

	// Configure regular health checks based on the standalone flag
	if !*standaloneHealthChecks && *healthCheckInterval > 0 {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		checker := health.NewChecker(cfg, redisClient, time.Duration(*healthCheckInterval)*time.Second, time.Duration(*ephemeralChecksInterval)*time.Second, *ephemeralChecksHealthyThreshold)

		log.Info().Int("interval_seconds", *healthCheckInterval).Msg("Starting integrated health check service")
		go checker.Start(ctx)
	} else if *standaloneHealthChecks {
		log.Info().Msg("Standalone health checks enabled (STANDALONE_HEALTH_CHECKS=true). Using external health checker service.")
	}

	// Initialize and start the server
	srv := server.NewServer(cfg, redisClient)
	if *metricsEnabled {
		srv.AddMiddleware(metrics.Middleware)
	}

	// Handle graceful shutdown
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	go func() {
		if err := srv.Start(*serverPort); err != nil {
			log.Fatal().Err(err).Msg("Server failed to start")
		}
	}()

	<-stop
	log.Info().Msg("Shutting down server...")
	if err := srv.Shutdown(); err != nil {
		log.Error().Err(err).Msg("Error during server shutdown")
	}
}
