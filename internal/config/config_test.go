package config

import (
	"os"
	"testing"
)

func TestLoad_Defaults(t *testing.T) {
	// Clear any env vars that might interfere
	os.Unsetenv("NATS_URL")
	os.Unsetenv("NEPHTYS_PORT")
	os.Unsetenv("NEPHTYS_LOG_LEVEL")

	cfg := Load()

	if cfg.NatsURL != "nats://localhost:4222" {
		t.Errorf("NatsURL: got %q, want default", cfg.NatsURL)
	}
	if cfg.Port != "3000" {
		t.Errorf("Port: got %q, want '3000'", cfg.Port)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel: got %q, want 'info'", cfg.LogLevel)
	}
}

func TestLoad_FromEnv(t *testing.T) {
	os.Setenv("NATS_URL", "nats://custom:4222")
	os.Setenv("NEPHTYS_PORT", "8080")
	os.Setenv("NEPHTYS_LOG_LEVEL", "debug")
	defer func() {
		os.Unsetenv("NATS_URL")
		os.Unsetenv("NEPHTYS_PORT")
		os.Unsetenv("NEPHTYS_LOG_LEVEL")
	}()

	cfg := Load()

	if cfg.NatsURL != "nats://custom:4222" {
		t.Errorf("NatsURL: got %q, want 'nats://custom:4222'", cfg.NatsURL)
	}
	if cfg.Port != "8080" {
		t.Errorf("Port: got %q, want '8080'", cfg.Port)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel: got %q, want 'debug'", cfg.LogLevel)
	}
}
