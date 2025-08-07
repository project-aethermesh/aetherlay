package helpers

import (
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/rs/zerolog/log"
)

// GetBoolFromEnv gets a string value from an env var and returns it as a boolean.
// If the env var is not found, a default value is returned.
// Accepts "true", "1", "yes" and "on" as true values (case insensitive).
// Accepts "false", "0", "no" and "off" as false values (case insensitive).
// If an invalid value is provided, it logs a warning and returns the default value.
func GetBoolFromEnv(key string, defaultValue bool) bool {
	if envVal := os.Getenv(key); envVal != "" {
		envVal = strings.ToLower(strings.TrimSpace(envVal))
		switch envVal {
		case "true", "1", "yes", "on":
			return true
		case "false", "0", "no", "off":
			return false
		default:
			log.Warn().Msg(envVal + " is an invalid boolean value for " + key + ", defaulting to: " + strconv.FormatBool(defaultValue))
			os.Setenv(key, strconv.FormatBool(defaultValue))
		}
	} else {
		log.Warn().Msg("Missing " + key + " from env vars, defaulting to: " + strconv.FormatBool(defaultValue))
		os.Setenv(key, strconv.FormatBool(defaultValue))
	}
	return defaultValue
}

// GetIntFromEnv gets a string value from an env var and returns it as an integer.
// If the env var is not found, a default value is returned.
// Integers are expected to be greater than or equal to zero.
// If an invalid value is provided, it logs a warning and returns the default value.
func GetIntFromEnv(key string, defaultValue int) int {
	if envVal := os.Getenv(key); envVal != "" {
		if parsed, err := strconv.Atoi(envVal); err == nil && parsed >= 0 {
			return parsed
		} else {
			log.Warn().Msg(envVal + "is an invalid value for " + key + ", defaulting to: " + strconv.Itoa(defaultValue))
			os.Setenv(key, strconv.Itoa(defaultValue))
		}
	} else {
		log.Warn().Msg("Missing " + key + " from env vars, defaulting to: " + strconv.Itoa(defaultValue))
		os.Setenv(key, strconv.Itoa(defaultValue))
	}
	return defaultValue
}

// GetStringFromEnv gets a string value from an env var and returns it as a string.
// If the env var is not found, a default value is returned.
// Empty strings are treated as missing values and will trigger the default.
func GetStringFromEnv(key string, defaultValue string) string {
	if envVal := os.Getenv(key); envVal != "" {
		if strings.TrimSpace(envVal) != "" {
			return envVal
		} else {
			log.Warn().Msg("Empty value for " + key + ", defaulting to: " + defaultValue)
			os.Setenv(key, defaultValue)
		}
	} else {
		log.Warn().Msg("Missing " + key + " from env vars, defaulting to: " + defaultValue)
		os.Setenv(key, defaultValue)
	}
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
