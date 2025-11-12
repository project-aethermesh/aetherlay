package helpers

import (
	"flag"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/rs/zerolog/log"
)

// Config holds all CLI flags and their values
type Config struct {
	ConfigFile                      string
	CorsHeaders                     string
	CorsOrigin                      string
	CorsMethods                     string
	EndpointFailureThreshold        int
	EndpointSuccessThreshold        int
	EphemeralChecksHealthyThreshold int
	EphemeralChecksInterval         int
	HealthCacheTTL                  int
	HealthCheckConcurrency          int
	HealthCheckInterval             int
	HealthCheckSyncStatus           bool
	LogLevel                        string
	MetricsEnabled                  bool
	MetricsPort                     int
	ProxyMaxRetries                 int
	ProxyTimeout                    int
	ProxyTimeoutPerTry              int
	PublicFirst                     bool
	PublicFirstAttempts             int
	ServerPort                      int
	StandaloneHealthChecks          bool
	ValkeyHost                      string
	ValkeyPass                      string
	ValkeyPort                      string
	ValkeySkipTLSCheck              bool
	ValkeyUseTLS                    bool
}

// ParseFlags defines and parses all CLI flags, returning a Config struct
func ParseFlags() *Config {
	config := &Config{}

	// Define all flags with proper defaults
	flag.StringVar(&config.ConfigFile, "config-file", "configs/endpoints.json", "Configuration file path")
	flag.StringVar(&config.CorsHeaders, "cors-headers", "Accept, Authorization, Content-Type, Origin, X-Requested-With", "CORS allowed headers")
	flag.StringVar(&config.CorsMethods, "cors-methods", "GET, POST, OPTIONS", "CORS allowed methods")
	flag.StringVar(&config.CorsOrigin, "cors-origin", "*", "CORS allowed origin")
	flag.IntVar(&config.EndpointFailureThreshold, "endpoint-failure-threshold", 2, "Number of consecutive failures before marking endpoint unhealthy")
	flag.IntVar(&config.EndpointSuccessThreshold, "endpoint-success-threshold", 2, "Number of consecutive successes before marking endpoint healthy")
	flag.IntVar(&config.EphemeralChecksHealthyThreshold, "ephemeral-checks-healthy-threshold", 3, "Ephemeral checks healthy threshold")
	flag.IntVar(&config.EphemeralChecksInterval, "ephemeral-checks-interval", 30, "Ephemeral checks interval in seconds")
	flag.IntVar(&config.HealthCacheTTL, "health-cache-ttl", 10, "Health status cache TTL in seconds")
	flag.IntVar(&config.HealthCheckConcurrency, "health-check-concurrency", 20, "Maximum number of concurrent health checks during startup")
	flag.IntVar(&config.HealthCheckInterval, "health-check-interval", 30, "Health check interval in seconds")
	flag.BoolVar(&config.HealthCheckSyncStatus, "health-check-sync-status", true, "Consider the sync status of the endpoints when deciding whether an endpoint is healthy or not.")
	flag.StringVar(&config.LogLevel, "log-level", "info", "Log level (debug, info, warn, error)")
	flag.BoolVar(&config.MetricsEnabled, "metrics-enabled", true, "Enable metrics server")
	flag.IntVar(&config.MetricsPort, "metrics-port", 9090, "Metrics server port")
	flag.IntVar(&config.ProxyMaxRetries, "proxy-retries", 3, "Maximum number of retries for proxy requests")
	flag.IntVar(&config.ProxyTimeout, "proxy-timeout", 15, "Timeout for proxy requests in seconds")
	flag.IntVar(&config.ProxyTimeoutPerTry, "proxy-timeout-per-try", 5, "Timeout per individual retry attempt in seconds")
	flag.BoolVar(&config.PublicFirst, "public-first", false, "Prioritize public endpoints over primary endpoints")
	flag.IntVar(&config.PublicFirstAttempts, "public-first-attempts", 2, "Number of attempts to make at public endpoints before trying primary/fallback")
	flag.IntVar(&config.ServerPort, "server-port", 8080, "Server port")
	flag.BoolVar(&config.StandaloneHealthChecks, "standalone-health-checks", true, "Enable standalone health checks")
	flag.StringVar(&config.ValkeyHost, "valkey-host", "localhost", "Valkey host")
	flag.StringVar(&config.ValkeyPass, "valkey-pass", "", "Valkey password")
	flag.StringVar(&config.ValkeyPort, "valkey-port", "6379", "Valkey port")
	flag.BoolVar(&config.ValkeySkipTLSCheck, "valkey-skip-tls-check", false, "Skip TLS certificate validation for Valkey")
	flag.BoolVar(&config.ValkeyUseTLS, "valkey-use-tls", false, "Use TLS for Valkey connection")

	// Parse the flags
	flag.Parse()

	log.Debug().Msg("CLI flags parsed successfully")
	return config
}

// GetStringValue returns the flag value if explicitly set, otherwise the env var value, otherwise the default
func (c *Config) GetStringValue(flagName string, flagValue string, envKey string, defaultValue string) string {
	// Check if the flag was explicitly set by looking it up
	if f := flag.Lookup(flagName); f != nil && f.Value.String() != f.DefValue {
		logValue := flagValue
		if flagName == "valkey-pass" {
			logValue = "REDACTED"
		}
		log.Debug().Str(flagName, logValue).Msg("Using value from flag")
		return flagValue
	}
	return getStringFromEnv(envKey, defaultValue)
}

// GetIntValue returns the flag value if explicitly set, otherwise the env var value, otherwise the default
func (c *Config) GetIntValue(flagName string, flagValue int, envKey string, defaultValue int) int {
	// Check if the flag was explicitly set by looking it up
	if f := flag.Lookup(flagName); f != nil && f.Value.String() != f.DefValue {
		log.Debug().Int(flagName, flagValue).Msg("Using value from flag")
		return flagValue
	}
	return getIntFromEnv(envKey, defaultValue)
}

// GetBoolValue returns the flag value if the flag was explicitly set, otherwise the env var value, otherwise the default
func (c *Config) GetBoolValue(flagName string, flagValue bool, envKey string, defaultValue bool) bool {
	// Check if the flag was explicitly set by looking it up
	if f := flag.Lookup(flagName); f != nil && f.Value.String() != f.DefValue {
		log.Debug().Bool(flagName, flagValue).Msg("Using value from flag")
		return flagValue
	}
	return getBoolFromEnv(envKey, defaultValue)
}

// LoadConfiguration loads all configuration values with proper precedence
func (c *Config) LoadConfiguration() *LoadedConfig {
	return &LoadedConfig{
		ConfigFile:                      c.GetStringValue("config-file", c.ConfigFile, "CONFIG_FILE", "configs/endpoints.json"),
		CorsHeaders:                     c.GetStringValue("cors-headers", c.CorsHeaders, "CORS_HEADERS", "Accept, Authorization, Content-Type, Origin, X-Requested-With"),
		CorsMethods:                     c.GetStringValue("cors-methods", c.CorsMethods, "CORS_METHODS", "GET, POST, OPTIONS"),
		CorsOrigin:                      c.GetStringValue("cors-origin", c.CorsOrigin, "CORS_ORIGIN", "*"),
		EndpointFailureThreshold:        c.GetIntValue("endpoint-failure-threshold", c.EndpointFailureThreshold, "ENDPOINT_FAILURE_THRESHOLD", 2),
		EndpointSuccessThreshold:        c.GetIntValue("endpoint-success-threshold", c.EndpointSuccessThreshold, "ENDPOINT_SUCCESS_THRESHOLD", 2),
		EphemeralChecksHealthyThreshold: c.GetIntValue("ephemeral-checks-healthy-threshold", c.EphemeralChecksHealthyThreshold, "EPHEMERAL_CHECKS_HEALTHY_THRESHOLD", 3),
		EphemeralChecksInterval:         c.GetIntValue("ephemeral-checks-interval", c.EphemeralChecksInterval, "EPHEMERAL_CHECKS_INTERVAL", 30),
		HealthCacheTTL:                  c.GetIntValue("health-cache-ttl", c.HealthCacheTTL, "HEALTH_CACHE_TTL", 10),
		HealthCheckConcurrency:          c.GetIntValue("health-check-concurrency", c.HealthCheckConcurrency, "HEALTH_CHECK_CONCURRENCY", 20),
		HealthCheckInterval:             c.GetIntValue("health-check-interval", c.HealthCheckInterval, "HEALTH_CHECK_INTERVAL", 30),
		HealthCheckSyncStatus:           c.GetBoolValue("health-check-sync-status", c.HealthCheckSyncStatus, "HEALTH_CHECK_SYNC_STATUS", true),
		LogLevel:                        c.GetStringValue("log-level", c.LogLevel, "LOG_LEVEL", "info"),
		MetricsEnabled:                  c.GetBoolValue("metrics-enabled", c.MetricsEnabled, "METRICS_ENABLED", true),
		MetricsPort:                     c.GetIntValue("metrics-port", c.MetricsPort, "METRICS_PORT", 9090),
		ProxyMaxRetries:                 c.GetIntValue("proxy-retries", c.ProxyMaxRetries, "PROXY_MAX_RETRIES", 3),
		ProxyTimeout:                    c.GetIntValue("proxy-timeout", c.ProxyTimeout, "PROXY_TIMEOUT", 15),
		ProxyTimeoutPerTry:              c.GetIntValue("proxy-timeout-per-try", c.ProxyTimeoutPerTry, "PROXY_TIMEOUT_PER_TRY", 5),
		PublicFirst:                     c.GetBoolValue("public-first", c.PublicFirst, "PUBLIC_FIRST", false),
		PublicFirstAttempts:             c.GetIntValue("public-first-attempts", c.PublicFirstAttempts, "PUBLIC_FIRST_ATTEMPTS", 2),
		ServerPort:                      c.GetIntValue("server-port", c.ServerPort, "SERVER_PORT", 8080),
		StandaloneHealthChecks:          c.GetBoolValue("standalone-health-checks", c.StandaloneHealthChecks, "STANDALONE_HEALTH_CHECKS", true),
		ValkeyHost:                      c.GetStringValue("valkey-host", c.ValkeyHost, "VALKEY_HOST", "localhost"),
		ValkeyPass:                      c.GetStringValue("valkey-pass", c.ValkeyPass, "VALKEY_PASS", ""),
		ValkeyPort:                      c.GetStringValue("valkey-port", c.ValkeyPort, "VALKEY_PORT", "6379"),
		ValkeySkipTLSCheck:              c.GetBoolValue("valkey-skip-tls-check", c.ValkeySkipTLSCheck, "VALKEY_SKIP_TLS_CHECK", false),
		ValkeyUseTLS:                    c.GetBoolValue("valkey-use-tls", c.ValkeyUseTLS, "VALKEY_USE_TLS", false),
	}
}

// LoadedConfig contains the final resolved configuration values
type LoadedConfig struct {
	ConfigFile                      string
	CorsHeaders                     string
	CorsMethods                     string
	CorsOrigin                      string
	EndpointFailureThreshold        int
	EndpointSuccessThreshold        int
	EphemeralChecksHealthyThreshold int
	EphemeralChecksInterval         int
	HealthCacheTTL                  int
	HealthCheckConcurrency          int
	HealthCheckInterval             int
	HealthCheckSyncStatus           bool
	LogLevel                        string
	MetricsEnabled                  bool
	MetricsPort                     int
	ProxyMaxRetries                 int
	ProxyTimeout                    int
	ProxyTimeoutPerTry              int
	PublicFirst                     bool
	PublicFirstAttempts             int
	ServerPort                      int
	StandaloneHealthChecks          bool
	ValkeyHost                      string
	ValkeyPass                      string
	ValkeyPort                      string
	ValkeySkipTLSCheck              bool
	ValkeyUseTLS                    bool
}

// Internal helper functions for environment variable processing

// getStringFromEnv gets a string value from an environment variable or returns a default
func getStringFromEnv(envKey, defaultValue string) string {
	if envValue := os.Getenv(envKey); envValue != "" {
		if strings.TrimSpace(envValue) != "" {
			logValue := envValue
			if envKey == "VALKEY_PASS" {
				logValue = "REDACTED"
			}
			log.Debug().Str(envKey, logValue).Msg("Parsed string value from env var")
			return envValue
		} else {
			log.Info().Msg("Empty value for " + envKey + ", defaulting to: " + defaultValue)
		}
	} else {
		log.Info().Msg("Missing " + envKey + " from env vars, defaulting to: " + defaultValue)
	}
	os.Setenv(envKey, defaultValue)
	return defaultValue
}

// getIntFromEnv gets an integer value from an environment variable or returns a default
func getIntFromEnv(envKey string, defaultValue int) int {
	if envValue := os.Getenv(envKey); envValue != "" {
		if parsed, err := strconv.Atoi(envValue); err == nil && parsed >= 0 {
			log.Debug().Int(envKey, parsed).Msg("Parsed integer value from env var")
			return parsed
		} else {
			log.Info().Msg(envValue + " is an invalid value for " + envKey + ", defaulting to: " + strconv.Itoa(defaultValue))
		}
	} else {
		log.Info().Msg("Missing " + envKey + " from env vars, defaulting to: " + strconv.Itoa(defaultValue))
	}
	os.Setenv(envKey, strconv.Itoa(defaultValue))
	return defaultValue
}

// getBoolFromEnv gets a boolean value from an environment variable or returns a default
func getBoolFromEnv(envKey string, defaultValue bool) bool {
	if envValue := os.Getenv(envKey); envValue != "" {
		envValue = strings.TrimSpace(envValue)
		if parsed, err := strconv.ParseBool(envValue); err == nil {
			log.Debug().Bool(envKey, parsed).Msg("Parsed boolean value from env var")
			return parsed
		} else {
			log.Info().Msg(envValue + " is an invalid boolean value for " + envKey + ", defaulting to: " + strconv.FormatBool(defaultValue))
		}
	} else {
		log.Info().Msg("Missing " + envKey + " from env vars, defaulting to: " + strconv.FormatBool(defaultValue))
	}
	os.Setenv(envKey, strconv.FormatBool(defaultValue))
	return defaultValue
}

// RedactAPIKey redacts API keys that would otherwise be shown in plain text in the logs.
// It matches common API key patterns in URLs and replaces them with a redacted version.
// For keys longer than 8 characters, it shows the first 4 and last 4 characters.
// For shorter keys, it completely redacts them.
func RedactAPIKey(url string) string {
	// Define patterns for different providers
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`(ankr\.com/(?:premium-http/)?[a-z0-9_-]+/)([A-Za-z0-9_-]+)`),
		regexp.MustCompile(`([a-z0-9-]+\.quiknode\.pro/)([A-Za-z0-9_-]+)`),
		regexp.MustCompile(`(infura\.io/(?:ws/)?v3/)([A-Za-z0-9_-]+)`),
		regexp.MustCompile(`(alchemy\.com/v2/)([A-Za-z0-9_-]+)`),
	}

	result := url
	for _, re := range patterns {
		result = re.ReplaceAllStringFunc(result, func(match string) string {
			parts := re.FindStringSubmatch(match)
			if len(parts) != 3 {
				return match
			}
			prefix, key := parts[1], parts[2]
			if len(key) <= 8 {
				return prefix + "..."
			}
			return prefix + key[:4] + "..." + key[len(key)-4:]
		})
	}
	return result
}
