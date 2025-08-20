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
	CorsMethods                     string
	CorsOrigin                      string
	EphemeralChecksHealthyThreshold int
	EphemeralChecksInterval         int
	HealthCheckInterval             int
	LogLevel                        string
	MetricsEnabled                  bool
	MetricsPort                     int
	RedisHost                       string
	RedisPass                       string
	RedisPort                       string
	RedisSkipTLSCheck               bool
	RedisUseTLS                     bool
	ServerPort                      int
	StandaloneHealthChecks          bool
}

// ParseFlags defines and parses all CLI flags, returning a Config struct
func ParseFlags() *Config {
	config := &Config{}

	// Define all flags with proper defaults
	flag.StringVar(&config.ConfigFile, "config-file", "configs/endpoints.json", "Configuration file path")
	flag.StringVar(&config.CorsHeaders, "cors-headers", "Accept, Authorization, Content-Type, Origin, X-Requested-With", "CORS allowed headers")
	flag.StringVar(&config.CorsMethods, "cors-methods", "GET, POST, OPTIONS", "CORS allowed methods")
	flag.StringVar(&config.CorsOrigin, "cors-origin", "*", "CORS allowed origin")
	flag.IntVar(&config.EphemeralChecksHealthyThreshold, "ephemeral-checks-healthy-threshold", 3, "Ephemeral checks healthy threshold")
	flag.IntVar(&config.EphemeralChecksInterval, "ephemeral-checks-interval", 30, "Ephemeral checks interval in seconds")
	flag.IntVar(&config.HealthCheckInterval, "health-check-interval", 30, "Health check interval in seconds")
	flag.StringVar(&config.LogLevel, "log-level", "info", "Log level (debug, info, warn, error)")
	flag.BoolVar(&config.MetricsEnabled, "metrics-enabled", true, "Enable metrics server")
	flag.IntVar(&config.MetricsPort, "metrics-port", 9090, "Metrics server port")
	flag.StringVar(&config.RedisHost, "redis-host", "localhost", "Redis host")
	flag.StringVar(&config.RedisPass, "redis-pass", "", "Redis password")
	flag.StringVar(&config.RedisPort, "redis-port", "6379", "Redis port")
	flag.BoolVar(&config.RedisSkipTLSCheck, "redis-skip-tls-check", false, "Skip TLS certificate validation for Redis")
	flag.BoolVar(&config.RedisUseTLS, "redis-use-tls", false, "Use TLS for Redis connection")
	flag.IntVar(&config.ServerPort, "server-port", 8080, "Server port")
	flag.BoolVar(&config.StandaloneHealthChecks, "standalone-health-checks", true, "Enable standalone health checks")

	// Parse the flags
	flag.Parse()

	log.Debug().Msg("CLI flags parsed successfully")
	return config
}

// GetStringValue returns the flag value if explicitly set, otherwise the env var value, otherwise the default
func (c *Config) GetStringValue(flagName, flagValue, envKey, defaultValue string) string {
	// Check if the flag was explicitly set by looking it up
	if f := flag.Lookup(flagName); f != nil && f.Value.String() != f.DefValue {
		log.Debug().Str(flagName, flagValue).Msg("Using value from flag")
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
		EphemeralChecksHealthyThreshold: c.GetIntValue("ephemeral-checks-healthy-threshold", c.EphemeralChecksHealthyThreshold, "EPHEMERAL_CHECKS_HEALTHY_THRESHOLD", 3),
		EphemeralChecksInterval:         c.GetIntValue("ephemeral-checks-interval", c.EphemeralChecksInterval, "EPHEMERAL_CHECKS_INTERVAL", 30),
		HealthCheckInterval:             c.GetIntValue("health-check-interval", c.HealthCheckInterval, "HEALTH_CHECK_INTERVAL", 30),
		LogLevel:                        c.GetStringValue("log-level", c.LogLevel, "LOG_LEVEL", "info"),
		MetricsEnabled:                  c.GetBoolValue("metrics-enabled", c.MetricsEnabled, "METRICS_ENABLED", true),
		MetricsPort:                     c.GetIntValue("metrics-port", c.MetricsPort, "METRICS_PORT", 9090),
		RedisHost:                       c.GetStringValue("redis-host", c.RedisHost, "REDIS_HOST", "localhost"),
		RedisPass:                       c.GetStringValue("redis-pass", c.RedisPass, "REDIS_PASS", ""),
		RedisPort:                       c.GetStringValue("redis-port", c.RedisPort, "REDIS_PORT", "6379"),
		RedisSkipTLSCheck:               c.GetBoolValue("redis-skip-tls-check", c.RedisSkipTLSCheck, "REDIS_SKIP_TLS_CHECK", false),
		RedisUseTLS:                     c.GetBoolValue("redis-use-tls", c.RedisUseTLS, "REDIS_USE_TLS", false),
		ServerPort:                      c.GetIntValue("server-port", c.ServerPort, "SERVER_PORT", 8080),
		StandaloneHealthChecks:          c.GetBoolValue("standalone-health-checks", c.StandaloneHealthChecks, "STANDALONE_HEALTH_CHECKS", true),
	}
}

// LoadedConfig contains the final resolved configuration values
type LoadedConfig struct {
	ConfigFile                      string
	CorsHeaders                     string
	CorsMethods                     string
	CorsOrigin                      string
	EphemeralChecksHealthyThreshold int
	EphemeralChecksInterval         int
	HealthCheckInterval             int
	LogLevel                        string
	MetricsEnabled                  bool
	MetricsPort                     int
	RedisHost                       string
	RedisPass                       string
	RedisPort                       string
	RedisSkipTLSCheck               bool
	RedisUseTLS                     bool
	ServerPort                      int
	StandaloneHealthChecks          bool
}

// GetMetricsPortForService returns the appropriate metrics port for the service.
// If the MetricsPort is set to its default value (9090) and the standalone health checks are enabled,
// the load balancer defaults to using port 9091 for metrics.
// This is to avoid conflicts when running the load balancer and the health checker on the same machine.
func (c *LoadedConfig) GetMetricsPortForService(isLoadBalancer bool) int {
	if isLoadBalancer && c.MetricsPort == 9090 && c.StandaloneHealthChecks {
		return 9091
	}
	return c.MetricsPort
}

// Internal helper functions for environment variable processing

// getStringFromEnv gets a string value from an environment variable or returns a default
func getStringFromEnv(envKey, defaultValue string) string {
	if envVal := os.Getenv(envKey); envVal != "" {
		if strings.TrimSpace(envVal) != "" {
			log.Debug().Str(envKey, envVal).Msg("Parsed string value from env var")
			return envVal
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
	if envVal := os.Getenv(envKey); envVal != "" {
		if parsed, err := strconv.Atoi(envVal); err == nil && parsed >= 0 {
			log.Debug().Int(envKey, parsed).Msg("Parsed integer value from env var")
			return parsed
		} else {
			log.Info().Msg(envVal + " is an invalid value for " + envKey + ", defaulting to: " + strconv.Itoa(defaultValue))
		}
	} else {
		log.Info().Msg("Missing " + envKey + " from env vars, defaulting to: " + strconv.Itoa(defaultValue))
	}
	os.Setenv(envKey, strconv.Itoa(defaultValue))
	return defaultValue
}

// getBoolFromEnv gets a boolean value from an environment variable or returns a default
func getBoolFromEnv(envKey string, defaultValue bool) bool {
	if envVal := os.Getenv(envKey); envVal != "" {
		envVal = strings.TrimSpace(envVal)
		if parsed, err := strconv.ParseBool(envVal); err == nil {
			log.Debug().Bool(envKey, parsed).Msg("Parsed boolean value from env var")
			return parsed
		} else {
			log.Info().Msg(envVal + " is an invalid boolean value for " + envKey + ", defaulting to: " + strconv.FormatBool(defaultValue))
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
// The regex can be greatly improved but, for now, it's enough for redacting keys from Alchemy and Infura.
func RedactAPIKey(url string) string {
	// Match common API key patterns in URLs
	// The regex can be greatly improved but, for now, it's enough for redacting keys from Alchemy and Infura
	re := regexp.MustCompile(`(v2/|v3/)([A-Za-z0-9]+)`)
	return re.ReplaceAllStringFunc(url, func(match string) string {
		parts := strings.Split(match, "/")
		if len(parts) != 2 {
			return match
		}
		prefix, key := parts[0], parts[1]
		if len(key) <= 8 {
			return prefix + "/..." // the key is too short to be redacted in this specific way, so we completely redact it
		}
		return prefix + "/" + key[:4] + "..." + key[len(key)-4:]
	})
}

// Legacy functions for backwards compatibility

// GetBoolFromFlagOrEnv gets a boolean value from a CLI flag or an environment variable.
// If the flag is set, it takes precedence over the environment variable.
// If neither is set, it returns a default value.
// The environment variable is expected to be "true" or "false" (case-insensitive).
// If an invalid value is provided, it logs a warning and returns the default value.
func GetBoolFromFlagOrEnv(flagKey string, envKey string, defaultValue bool) bool {
	log.Debug().Msg("Getting boolean value from flag " + flagKey + " or env var " + envKey)
	if cliFlag := flag.Lookup(flagKey); cliFlag != nil {
		if cliFlag.Value.String() != "" {
			if parsed, err := strconv.ParseBool(cliFlag.Value.String()); err == nil {
				log.Debug().Bool(flagKey, parsed).Msg("Parsed boolean value from flag")
				return parsed
			} else {
				log.Warn().Msg(cliFlag.Value.String() + " is an invalid boolean value for " + flagKey + ", trying to get it from the " + envKey + "env var...")
			}
		}
	}
	return getBoolFromEnv(envKey, defaultValue)
}

// GetStringFromFlagOrEnv gets a string value from a CLI flag or an environment variable.
// If the flag is set, it takes precedence over the environment variable.
// If neither is set, it returns a default value.
// Empty strings are treated as missing values and will trigger the default.
func GetStringFromFlagOrEnv(flagKey string, envKey string, defaultValue string) string {
	if cliFlag := flag.Lookup(flagKey); cliFlag != nil {
		if flagValue := cliFlag.Value.String(); strings.TrimSpace(flagValue) != "" {
			log.Debug().Str(flagKey, flagValue).Msg("Parsed string value from flag")
			return flagValue
		}
	}
	return getStringFromEnv(envKey, defaultValue)
}
