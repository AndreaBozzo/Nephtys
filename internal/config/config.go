// Package config handles environment-based configuration for Nephtys.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// Config holds all runtime configuration values.
type Config struct {
	NatsURL    string // NATS broker URL
	Port       string // REST API listen port
	LogLevel   string // Logging level
	AdminToken string // Bearer token for API auth (empty = disabled)
}

// Load reads configuration from environment variables with sensible defaults.
func Load() Config {
	return Config{
		NatsURL:    envOr("NATS_URL", "nats://localhost:4222"),
		Port:       envOr("NEPHTYS_PORT", "3002"),
		LogLevel:   envOr("NEPHTYS_LOG_LEVEL", "info"),
		AdminToken: os.Getenv("NEPHTYS_ADMIN_TOKEN"),
	}
}

// ParseLogLevel maps a NEPHTYS_LOG_LEVEL value to a slog level. Matching is
// case-insensitive and surrounding whitespace is ignored. Unrecognized values
// return LevelInfo along with an error, so callers can fall back to a working
// logger instead of refusing to start.
func ParseLogLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "", "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("unrecognized log level %q (want debug, info, warn, or error)", s)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
