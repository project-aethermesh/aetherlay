package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"aetherlay/internal/config"
	"aetherlay/internal/cors"
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

	// Parse CLI flags and load configuration
	flagConfig := helpers.ParseFlags()
	appConfig := flagConfig.LoadConfiguration()

	// Set the requested log level if it's valid, otherwise default to info
	if level, err := zerolog.ParseLevel(appConfig.LogLevel); err == nil {
		zerolog.SetGlobalLevel(level)
	} else {
		log.Warn().Str("LOG_LEVEL", appConfig.LogLevel).Msg("Invalid log level, defaulting to Info")
		zerolog.SetGlobalLevel(zerolog.InfoLevel)
	}

	// Start the metrics server if enabled
	if appConfig.MetricsEnabled {
		log.Info().Int("port", appConfig.MetricsPort).Msg("Prometheus metrics server enabled")
		metrics.StartServer(appConfig.MetricsPort, appConfig.CorsHeaders, appConfig.CorsMethods, appConfig.CorsOrigin)
	}

	// Load configuration
	cfg, err := config.LoadConfig(appConfig.ConfigFile)
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

	// Initialize Valkey client
	valkeyAddr := appConfig.ValkeyHost + ":" + appConfig.ValkeyPort
	valkeyClient := store.NewValkeyClient(valkeyAddr, appConfig.ValkeyPass, appConfig.ValkeySkipTLSCheck, appConfig.ValkeyUseTLS)
	if err := valkeyClient.Ping(context.Background()); err != nil {
		log.Fatal().Err(err).Msg("Failed to connect to Valkey")
	}

	// Initialize and start the server
	srv := server.NewServer(cfg, valkeyClient, appConfig)

	// Configure regular health checks based on the value of standaloneHealthChecks
	var checker *health.Checker
	if !appConfig.StandaloneHealthChecks {
		if appConfig.HealthCheckInterval > 0 {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			checker = health.NewChecker(cfg, valkeyClient, time.Duration(appConfig.HealthCheckInterval)*time.Second, time.Duration(appConfig.EphemeralChecksInterval)*time.Second, appConfig.EphemeralChecksHealthyThreshold, appConfig.HealthCheckSyncStatus)

			// Connect health checker to server's rate limit handler
			checker.HandleRateLimitFunc = srv.GetRateLimitHandler()

			log.Info().Int("interval_seconds", appConfig.HealthCheckInterval).Msg("Starting integrated health check service")
			go checker.Start(ctx)

			// Wait for initial health check to complete before accepting traffic
			log.Info().Msg("Waiting for initial health check to complete...")
			checker.WaitForInitialCheck()
			log.Info().Msg("Initial health check completed, ready to accept traffic")
		}
	} else if appConfig.StandaloneHealthChecks {
		log.Info().Msg("Standalone health checks enabled (STANDALONE_HEALTH_CHECKS=true). Using external health checker service.")
	}

	// Pass health checker to server for readiness endpoint
	if checker != nil {
		srv.SetHealthChecker(checker)
	}
	srv.AddMiddleware(func(next http.Handler) http.Handler {
		return cors.Middleware(next, appConfig.CorsHeaders, appConfig.CorsMethods, appConfig.CorsOrigin)
	})
	if appConfig.MetricsEnabled {
		srv.AddMiddleware(metrics.Middleware)
	}

	// Handle graceful shutdown
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	go func() {
		if err := srv.Start(appConfig.ServerPort); err != nil {
			log.Fatal().Err(err).Msg("Server failed to start")
		}
	}()

	<-stop
	log.Info().Msg("Shutting down server...")
	if err := srv.Shutdown(); err != nil {
		log.Error().Err(err).Msg("Error during server shutdown")
	}
}
