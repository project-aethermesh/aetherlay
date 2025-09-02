package config

import (
	"os"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	// Test loading a valid configuration file
	configFile := "../../configs/endpoints-example.json"
	config, err := LoadConfig(configFile)
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	if config == nil {
		t.Fatal("Config should not be nil")
	}

	// Check if mainnet chain exists
	mainnetEndpoints, exists := config.GetEndpointsForChain("mainnet")
	if !exists {
		t.Fatal("Mainnet chain should exist in config")
	}

	// Check if llama endpoint exists
	llamaEndpoint, exists := mainnetEndpoints["llama-1"]
	if !exists {
		t.Fatal("Llama endpoint should exist in mainnet chain")
	}

	if llamaEndpoint.Provider != "llama" {
		t.Errorf("Expected provider 'llama', got '%s'", llamaEndpoint.Provider)
	}

	if llamaEndpoint.Role != "primary" {
		t.Errorf("Expected role 'primary', got '%s'", llamaEndpoint.Role)
	}
}

func TestEnvironmentVariableSubstitution(t *testing.T) {
	// Set test environment variables
	os.Setenv("TEST_API_KEY", "test_key_123")
	os.Setenv("TEST_URL", "https://test.example.com")

	// Test substitution function
	result := substituteEnvVars("https://api.example.com/v2/${TEST_API_KEY}")
	expected := "https://api.example.com/v2/test_key_123"
	if result != expected {
		t.Errorf("Expected '%s', got '%s'", expected, result)
	}

	result = substituteEnvVars("${TEST_URL}/endpoint")
	expected = "https://test.example.com/endpoint"
	if result != expected {
		t.Errorf("Expected '%s', got '%s'", expected, result)
	}

	// Test with non-existent environment variable
	result = substituteEnvVars("https://api.example.com/v2/${NON_EXISTENT_KEY}")
	expected = "https://api.example.com/v2/"
	if result != expected {
		t.Errorf("Expected '%s', got '%s'", expected, result)
	}
}

func TestGetPrimaryEndpoints(t *testing.T) {
	configFile := "../../configs/endpoints-example.json"
	config, err := LoadConfig(configFile)
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	primaryEndpoints := config.GetPrimaryEndpoints("mainnet")
	if len(primaryEndpoints) == 0 {
		t.Fatal("Should have at least one primary endpoint for mainnet")
	}

	for _, endpoint := range primaryEndpoints {
		if endpoint.Role != "primary" {
			t.Errorf("Expected role 'primary', got '%s'", endpoint.Role)
		}
	}
}

func TestGetFallbackEndpoints(t *testing.T) {
	configFile := "../../configs/endpoints-example.json"
	config, err := LoadConfig(configFile)
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	fallbackEndpoints := config.GetFallbackEndpoints("mainnet")
	if len(fallbackEndpoints) == 0 {
		t.Fatal("Should have at least one fallback endpoint for mainnet")
	}

	for _, endpoint := range fallbackEndpoints {
		if endpoint.Role != "fallback" {
			t.Errorf("Expected role 'fallback', got '%s'", endpoint.Role)
		}
	}
}

func TestGetEndpointsForChain(t *testing.T) {
	configFile := "../../configs/endpoints-example.json"
	config, err := LoadConfig(configFile)
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	// Test existing chain
	endpoints, exists := config.GetEndpointsForChain("mainnet")
	if !exists {
		t.Fatal("Mainnet chain should exist")
	}
	if len(endpoints) == 0 {
		t.Fatal("Mainnet chain should have endpoints")
	}

	// Test non-existing chain
	endpoints, exists = config.GetEndpointsForChain("nonexistent")
	if exists {
		t.Fatal("Non-existent chain should not exist")
	}
	if len(endpoints) != 0 {
		t.Fatal("Non-existent chain should have no endpoints")
	}
}

func TestLoadConfigInvalidFile(t *testing.T) {
	_, err := LoadConfig("nonexistent.json")
	if err == nil {
		t.Fatal("Should return error for non-existent file")
	}
}

func TestLoadConfigInvalidJSON(t *testing.T) {
	// Create a temporary file with invalid JSON
	tmpFile := "test_invalid.json"
	content := `{"invalid": json}`
	err := os.WriteFile(tmpFile, []byte(content), 0644)
	if err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}
	defer os.Remove(tmpFile)

	_, err = LoadConfig(tmpFile)
	if err == nil {
		t.Fatal("Should return error for invalid JSON")
	}
}

func TestDefaultRateLimitRecovery(t *testing.T) {
	config := DefaultRateLimitRecovery()

	if config.CheckInterval != 60 {
		t.Errorf("Expected CheckInterval to be 60, got %d", config.CheckInterval)
	}

	if config.MaxRetries != 10 {
		t.Errorf("Expected MaxRetries to be 10, got %d", config.MaxRetries)
	}

	if config.RequiredSuccesses != 3 {
		t.Errorf("Expected RequiredSuccesses to be 3, got %d", config.RequiredSuccesses)
	}
}

func TestRateLimitRecoveryStructFields(t *testing.T) {
	recovery := RateLimitRecovery{
		CheckInterval:     120,
		MaxRetries:        5,
		RequiredSuccesses: 2,
	}

	if recovery.CheckInterval != 120 {
		t.Errorf("Expected CheckInterval to be 120, got %d", recovery.CheckInterval)
	}

	if recovery.MaxRetries != 5 {
		t.Errorf("Expected MaxRetries to be 5, got %d", recovery.MaxRetries)
	}

	if recovery.RequiredSuccesses != 2 {
		t.Errorf("Expected RequiredSuccesses to be 2, got %d", recovery.RequiredSuccesses)
	}
}

func TestEndpointWithRateLimitRecovery(t *testing.T) {
	recovery := &RateLimitRecovery{
		CheckInterval:     30,
		MaxRetries:        3,
		RequiredSuccesses: 1,
	}

	endpoint := Endpoint{
		Provider:          "test-provider",
		RateLimitRecovery: recovery,
		Role:              "primary",
		Type:              "full",
		HTTPURL:           "http://test.com",
		WSURL:             "ws://test.com",
	}

	if endpoint.RateLimitRecovery == nil {
		t.Fatal("Expected rate limit recovery configuration to be set")
	}

	if endpoint.RateLimitRecovery.CheckInterval != 30 {
		t.Errorf("Expected CheckInterval to be 30, got %d", endpoint.RateLimitRecovery.CheckInterval)
	}

	if endpoint.RateLimitRecovery.MaxRetries != 3 {
		t.Errorf("Expected MaxRetries to be 3, got %d", endpoint.RateLimitRecovery.MaxRetries)
	}

	if endpoint.RateLimitRecovery.RequiredSuccesses != 1 {
		t.Errorf("Expected RequiredSuccesses to be 1, got %d", endpoint.RateLimitRecovery.RequiredSuccesses)
	}
}
