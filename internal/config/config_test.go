package config

import (
	"os"
	"testing"
)

func TestLoad_Defaults(t *testing.T) {
	// Clear any env vars that might interfere
	for _, k := range []string{"NATS_URL", "NEPHTYS_PORT", "NEPHTYS_LOG_LEVEL", "NEPHTYS_ADMIN_TOKEN"} {
		_ = os.Unsetenv(k)
	}

	cfg := Load()

	if cfg.NatsURL != "nats://localhost:4222" {
		t.Errorf("NatsURL: got %q, want default", cfg.NatsURL)
	}
	if cfg.Port != "3002" {
		t.Errorf("Port: got %q, want '3002'", cfg.Port)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel: got %q, want 'info'", cfg.LogLevel)
	}
	if cfg.AdminToken != "" {
		t.Errorf("AdminToken: got %q, want empty", cfg.AdminToken)
	}
}

func TestLoad_FromEnv(t *testing.T) {
	t.Setenv("NATS_URL", "nats://custom:4222")
	t.Setenv("NEPHTYS_PORT", "8080")
	t.Setenv("NEPHTYS_LOG_LEVEL", "debug")
	t.Setenv("NEPHTYS_ADMIN_TOKEN", "secret-token")

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
	if cfg.AdminToken != "secret-token" {
		t.Errorf("AdminToken: got %q, want 'secret-token'", cfg.AdminToken)
	}
}
