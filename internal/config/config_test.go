package config

import (
	"log/slog"
	"os"
	"strings"
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

func TestParseLogLevel(t *testing.T) {
	tests := []struct {
		in      string
		want    slog.Level
		wantErr bool
	}{
		{"debug", slog.LevelDebug, false},
		{"DEBUG", slog.LevelDebug, false},
		{" Debug ", slog.LevelDebug, false},
		{"info", slog.LevelInfo, false},
		{"", slog.LevelInfo, false},
		{"warn", slog.LevelWarn, false},
		{"warning", slog.LevelWarn, false},
		{"error", slog.LevelError, false},
		{"ERROR", slog.LevelError, false},
		{"verbose", slog.LevelInfo, true},
		{"trace", slog.LevelInfo, true},
	}

	for _, tt := range tests {
		got, err := ParseLogLevel(tt.in)
		if got != tt.want {
			t.Errorf("ParseLogLevel(%q) level = %v, want %v", tt.in, got, tt.want)
		}
		if (err != nil) != tt.wantErr {
			t.Errorf("ParseLogLevel(%q) err = %v, wantErr %v", tt.in, err, tt.wantErr)
		}
	}
}

// An unrecognized level must still yield a usable level, so callers can install
// a working logger and merely warn rather than aborting startup.
func TestParseLogLevel_InvalidNamesTheValue(t *testing.T) {
	_, err := ParseLogLevel("verbose")
	if err == nil {
		t.Fatal("ParseLogLevel(\"verbose\") returned nil error")
	}
	if !strings.Contains(err.Error(), "verbose") {
		t.Errorf("error %q does not name the offending value", err.Error())
	}
}
