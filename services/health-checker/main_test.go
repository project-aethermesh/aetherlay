package main

import (
	"os"
	"testing"
)

func TestRunHealthCheckerFromEnv_Standalone(t *testing.T) {
	os.Setenv("STANDALONE_HEALTH_CHECKS", "true")
	os.Setenv("HEALTH_CHECK_INTERVAL", "30")
	t.Cleanup(func() {
		os.Unsetenv("STANDALONE_HEALTH_CHECKS")
		os.Unsetenv("HEALTH_CHECK_INTERVAL")
	})

	var detectedMode string
	onModeDetected = func(mode string) {
		detectedMode = mode
	}

	// Run main function
	main()

	if detectedMode != "standalone" {
		t.Errorf("Expected mode 'standalone', got '%s'", detectedMode)
	}
}

func TestRunHealthCheckerFromEnv_Ephemeral(t *testing.T) {
	os.Setenv("STANDALONE_HEALTH_CHECKS", "true")
	os.Setenv("HEALTH_CHECK_INTERVAL", "0")
	t.Cleanup(func() {
		os.Unsetenv("STANDALONE_HEALTH_CHECKS")
		os.Unsetenv("HEALTH_CHECK_INTERVAL")
	})

	var detectedMode string
	onModeDetected = func(mode string) {
		detectedMode = mode
	}

	// Run main function
	main()

	if detectedMode != "ephemeral" {
		t.Errorf("Expected mode 'ephemeral', got '%s'", detectedMode)
	}
}

func TestRunHealthCheckerFromEnv_Disabled(t *testing.T) {
	os.Setenv("STANDALONE_HEALTH_CHECKS", "false")
	os.Setenv("HEALTH_CHECK_INTERVAL", "0")
	t.Cleanup(func() {
		os.Unsetenv("STANDALONE_HEALTH_CHECKS")
		os.Unsetenv("HEALTH_CHECK_INTERVAL")
	})

	var detectedMode string
	onModeDetected = func(mode string) {
		detectedMode = mode
	}

	// Run main function
	main()

	if detectedMode != "disabled" {
		t.Errorf("Expected mode 'disabled', got '%s'", detectedMode)
	}
}
