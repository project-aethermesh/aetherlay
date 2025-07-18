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
	serverPort := flag.Int("server-port", helpers.GetIntFromEnv("SERVER_PORT", 8080), "Server port")
	standaloneHealthChecks := flag.Bool("standalone-health-checks", helpers.GetBoolFromEnv("STANDALONE_HEALTH_CHECKS", true), "Enable standalone health checks (overrides STANDALONE_HEALTH_CHECKS env var)")
	flag.Parse()

	// Get Redis password from the env var
	redisPassword := helpers.GetStringFromEnv("REDIS_PASS", "")

	// Load configuration
	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to load configuration")
	}

	// Print the loaded configuration to see the substitutions
	log.Info().Msg("RPC Load Balancer - Loaded configuration:")
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

	// Configure regular health checks based on the standalone flag
	if !*standaloneHealthChecks && *healthCheckInterval > 0 {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		checker := health.NewChecker(cfg, redisClient, time.Duration(*healthCheckInterval)*time.Second)

		log.Info().Int("interval_seconds", *healthCheckInterval).Msg("Starting integrated health check service")
		go checker.Start(ctx)
	} else if *standaloneHealthChecks && *healthCheckInterval > 0 {
		log.Info().Msg("Standalone health checks enabled (STANDALONE_HEALTH_CHECKS=true). Using external health checker service.")
	}

	// Initialize and start the server
	srv := server.NewServer(cfg, redisClient)

	// Run ephemeral health checks if regular health checks are disabled
	if *healthCheckInterval == 0 {
		log.Info().Msg("Ephemeral health checking enabled (HEALTH_CHECK_INTERVAL=0)")
		intervalSec := helpers.GetIntFromEnv("EPHEMERAL_CHECKS_INTERVAL", 30)
		srv.SetEphemeralCheckInterval(time.Duration(intervalSec) * time.Second)
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
