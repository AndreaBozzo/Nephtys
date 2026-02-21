// Package config handles environment-based configuration for Nephtys.
package config

import "os"

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

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
