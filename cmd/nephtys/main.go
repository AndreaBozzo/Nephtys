package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"nephtys/internal/broker"
	"nephtys/internal/config"
	"nephtys/internal/domain"
	"nephtys/internal/server"
	"nephtys/internal/store"
)

// version is the build-stamped version string. It is set at build time via
// -ldflags "-X main.version=v1.2.3". Falls back to runtime/debug VCS info,
// then to "dev" so local builds always print something useful.
var version = ""

func resolveVersion() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		var revision, modified string
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				revision = s.Value
			case "vcs.modified":
				modified = s.Value
			}
		}
		if revision != "" {
			short := revision
			if len(short) > 12 {
				short = short[:12]
			}
			if modified == "true" {
				return short + "-dirty"
			}
			return short
		}
	}
	return "dev"
}

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		showVersion bool
		configCheck string
	)
	flag.BoolVar(&showVersion, "version", false, "Print version and exit.")
	flag.StringVar(&configCheck, "config-check", "", "Validate a stream config JSON file (or '-' for stdin) and exit. Exit code 0 = valid, 1 = invalid.")
	flag.Parse()

	if showVersion {
		fmt.Printf("nephtys %s\n", resolveVersion())
		return nil
	}

	if configCheck != "" {
		return runConfigCheck(configCheck)
	}

	return runService()
}

// runConfigCheck reads a stream config from path (or stdin if path == "-")
// and validates it against the same rules used by POST /v1/streams.
func runConfigCheck(path string) error {
	const maxConfigBytes = 1 << 20 // 1 MiB; stream configs are tiny.

	var data []byte
	var err error
	if path == "-" {
		data, err = io.ReadAll(io.LimitReader(os.Stdin, maxConfigBytes+1))
		if err == nil && len(data) > maxConfigBytes {
			return fmt.Errorf("stdin payload exceeds %d bytes", maxConfigBytes)
		}
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}

	var cfg domain.StreamSourceConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	if err := server.ValidateStreamConfig(cfg); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}

	fmt.Printf("OK: %s (kind=%s, topic=%s)\n", cfg.ID, cfg.Kind, cfg.Topic)
	return nil
}

// configureLogging installs the process-wide slog handler at the requested
// level. An unrecognized level is not fatal: the process starts at info and
// says so once, since a typo in an env var is a poor reason to refuse to run.
func configureLogging(level string) {
	parsed, err := config.ParseLogLevel(level)
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: parsed})))
	if err != nil {
		slog.Warn("Falling back to info logging", "error", err)
	}
}

func runService() error {
	cfg := config.Load()
	configureLogging(cfg.LogLevel)

	slog.Info("Starting Nephtys Edge Connector", "version", resolveVersion())

	brk, err := broker.Connect(cfg.NatsURL, broker.DefaultConfig())
	if err != nil {
		return fmt.Errorf("connect to NATS: %w", err)
	}
	defer brk.Close()

	if err := brk.EnsureStream("NEPHTYS", []string{"nephtys.stream.>"}); err != nil {
		return fmt.Errorf("create JetStream stream: %w", err)
	}

	st, err := store.NewStreamStore(brk.JetStream())
	if err != nil {
		return fmt.Errorf("initialize config store: %w", err)
	}

	manager := server.NewStreamManager(brk, st)
	if err := manager.Restore(); err != nil {
		slog.Warn("Failed to restore streams", "error", err)
	}

	if cfg.AdminToken != "" {
		slog.Info("Admin auth enabled")
	} else {
		slog.Warn("Admin auth disabled (set NEPHTYS_ADMIN_TOKEN to enable)")
	}
	srv := server.New(cfg.Port, manager, brk, cfg.AdminToken)
	srvErr := make(chan error, 1)
	go func() {
		if err := srv.Start(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			srvErr <- err
			return
		}
		srvErr <- nil
	}()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-sigs:
		slog.Info("Shutting down gracefully...")
	case err := <-srvErr:
		if err != nil {
			return fmt.Errorf("server error: %w", err)
		}
		return nil
	}

	manager.StopAll()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("Server shutdown error", "error", err)
	}

	slog.Info("Nephtys terminated")
	return nil
}
