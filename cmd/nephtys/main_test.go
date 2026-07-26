package main

import (
	"context"
	"flag"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
)

func TestResolveVersion_OverrideWins(t *testing.T) {
	// Save and restore the package-level version variable.
	orig := version
	t.Cleanup(func() { version = orig })

	version = "v1.2.3"
	if got := resolveVersion(); got != "v1.2.3" {
		t.Errorf("resolveVersion() with override = %q, want %q", got, "v1.2.3")
	}
}

func TestResolveVersion_FallbackNonEmpty(t *testing.T) {
	orig := version
	t.Cleanup(func() { version = orig })

	version = ""
	got := resolveVersion()
	if got == "" {
		t.Fatal("resolveVersion() with no override returned empty string")
	}
	// Either a 12-char-or-shorter VCS revision (optionally suffixed with -dirty),
	// or the literal "dev" if no VCS info is available. Just assert a sane shape.
	if got != "dev" && len(got) > 32 {
		t.Errorf("resolveVersion() = %q, expected short VCS revision or 'dev'", got)
	}
}

func TestRunConfigCheck_ValidFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "valid.json")
	const cfg = `{
		"id": "test_stream",
		"kind": "rest_poller",
		"url": "https://example.com/api",
		"topic": "nephtys.stream.test"
	}`
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}

	if err := runConfigCheck(path); err != nil {
		t.Errorf("runConfigCheck(valid file) returned error: %v", err)
	}
}

func TestRunConfigCheck_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(path, []byte("not-json{"), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}

	err := runConfigCheck(path)
	if err == nil {
		t.Fatal("runConfigCheck(bad json) returned nil, want error")
	}
	if !strings.Contains(err.Error(), "parse config") {
		t.Errorf("error %q does not mention parse failure", err.Error())
	}
}

func TestRunConfigCheck_InvalidScheme(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad-scheme.json")
	const cfg = `{
		"id": "x",
		"kind": "websocket",
		"url": "http://wrong-scheme.example",
		"topic": "nephtys.stream.x"
	}`
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}

	err := runConfigCheck(path)
	if err == nil {
		t.Fatal("runConfigCheck(bad scheme) returned nil, want error")
	}
	if !strings.Contains(err.Error(), "invalid config") {
		t.Errorf("error %q does not mention validation failure", err.Error())
	}
}

func TestRunConfigCheck_MissingFile(t *testing.T) {
	err := runConfigCheck(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err == nil {
		t.Fatal("runConfigCheck(missing) returned nil, want error")
	}
	if !strings.Contains(err.Error(), "read config") {
		t.Errorf("error %q does not mention read failure", err.Error())
	}
}

// TestRunConfigCheck_Stdin verifies the '-' path by replacing os.Stdin with a
// pipe whose write end is closed after writing the payload — equivalent to a
// shell heredoc. We restore os.Stdin via t.Cleanup to keep parallel tests safe.
func TestRunConfigCheck_Stdin(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	go func() {
		_, _ = w.Write([]byte(`{
			"id": "stdin_test",
			"kind": "sse",
			"url": "https://example.com/events",
			"topic": "nephtys.stream.stdin"
		}`))
		_ = w.Close()
	}()

	origStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = origStdin })

	if err := runConfigCheck("-"); err != nil {
		t.Errorf("runConfigCheck(stdin) returned error: %v", err)
	}
}

// withArgs swaps os.Args + the global flag.CommandLine for the duration of
// a sub-call so we can exercise run() without leaking flag state into other
// tests. Restores everything via cleanup.
func withArgs(t *testing.T, args []string) {
	t.Helper()
	origArgs := os.Args
	origFlag := flag.CommandLine
	os.Args = append([]string{"nephtys"}, args...)
	flag.CommandLine = flag.NewFlagSet("nephtys", flag.ContinueOnError)
	flag.CommandLine.SetOutput(io.Discard)
	t.Cleanup(func() {
		os.Args = origArgs
		flag.CommandLine = origFlag
	})
}

func TestRun_VersionFlag(t *testing.T) {
	withArgs(t, []string{"--version"})
	if err := run(); err != nil {
		t.Errorf("run(--version) returned error: %v", err)
	}
}

func TestRun_ConfigCheckFlag(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")
	const cfg = `{
		"id": "run_test",
		"kind": "websocket",
		"url": "wss://example.com/ws",
		"topic": "nephtys.stream.run"
	}`
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}

	withArgs(t, []string{"--config-check", path})
	if err := run(); err != nil {
		t.Errorf("run(--config-check valid) returned error: %v", err)
	}
}

func TestRun_ConfigCheckFlag_Invalid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(path, []byte("garbage"), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}

	withArgs(t, []string{"--config-check", path})
	if err := run(); err == nil {
		t.Fatal("run(--config-check garbage) returned nil, want error")
	}
}

func TestRunConfigCheck_StdinTooLarge(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	// Send 2 MiB — over the 1 MiB cap.
	huge := make([]byte, 2<<20)
	for i := range huge {
		huge[i] = '{' // junk; we never get to parse it.
	}
	go func() {
		_, _ = w.Write(huge)
		_ = w.Close()
	}()

	origStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = origStdin })

	err = runConfigCheck("-")
	if err == nil {
		t.Fatal("runConfigCheck(huge stdin) returned nil, want error")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error %q does not mention size limit", err.Error())
	}
}

// withDefaultLogger restores the process-wide slog default after a test that
// calls configureLogging, which necessarily mutates global state.
func withDefaultLogger(t *testing.T) {
	t.Helper()
	orig := slog.Default()
	t.Cleanup(func() { slog.SetDefault(orig) })
}

func TestConfigureLogging_AppliesLevel(t *testing.T) {
	tests := []struct {
		level    string
		enabled  []slog.Level
		disabled []slog.Level
	}{
		{"debug", []slog.Level{slog.LevelDebug, slog.LevelInfo}, nil},
		{"info", []slog.Level{slog.LevelInfo}, []slog.Level{slog.LevelDebug}},
		{"warn", []slog.Level{slog.LevelWarn}, []slog.Level{slog.LevelInfo, slog.LevelDebug}},
		{"error", []slog.Level{slog.LevelError}, []slog.Level{slog.LevelWarn, slog.LevelInfo}},
		// Unrecognized values must not disable logging altogether.
		{"verbose", []slog.Level{slog.LevelInfo}, []slog.Level{slog.LevelDebug}},
	}

	for _, tt := range tests {
		withDefaultLogger(t)
		// Captured, not written: the invalid case warns, and that warning is
		// asserted elsewhere — here it would just be noise in `go test` output.
		_ = captureStderr(t, func() { configureLogging(tt.level) })

		for _, lvl := range tt.enabled {
			if !slog.Default().Enabled(context.Background(), lvl) {
				t.Errorf("configureLogging(%q): %v should be enabled", tt.level, lvl)
			}
		}
		for _, lvl := range tt.disabled {
			if slog.Default().Enabled(context.Background(), lvl) {
				t.Errorf("configureLogging(%q): %v should be suppressed", tt.level, lvl)
			}
		}
	}
}

// captureStderr redirects os.Stderr for the duration of fn and returns what was
// written to it. Restoration and descriptor closing are also registered with
// t.Cleanup, so an early t.Fatalf cannot leave the process's stderr pointing at
// an abandoned pipe or leak the descriptors.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	origStderr := os.Stderr
	os.Stderr = w
	t.Cleanup(func() {
		os.Stderr = origStderr
		_ = w.Close()
		_ = r.Close()
	})

	fn()

	// Close the write end before reading, otherwise ReadAll never sees EOF.
	os.Stderr = origStderr
	_ = w.Close()

	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read captured stderr: %v", err)
	}
	return string(out)
}

// The fallback path must say why it fell back, and name the bad value — a
// silent downgrade to info is how a mistyped env var goes unnoticed for weeks.
func TestConfigureLogging_InvalidWarnsOnce(t *testing.T) {
	withDefaultLogger(t)

	got := captureStderr(t, func() { configureLogging("verbose") })

	if !strings.Contains(got, "verbose") {
		t.Errorf("warning %q does not name the offending value", got)
	}
	if n := strings.Count(got, "Falling back"); n != 1 {
		t.Errorf("got %d fallback warnings, want exactly 1: %q", n, got)
	}
}

func TestConfigureLogging_ValidIsSilent(t *testing.T) {
	withDefaultLogger(t)

	if got := captureStderr(t, func() { configureLogging("debug") }); got != "" {
		t.Errorf("configureLogging(\"debug\") wrote %q, want no output", got)
	}
}

// runService is the only place where NEPHTYS_LOG_LEVEL actually reaches the
// running process, so cover the wiring end to end rather than just the helper.
//
// Terminating runService needs a failure it cannot recover from, since
// delivering SIGTERM to our own process is not portable. An out-of-range REST
// port makes net.Listen fail identically on every platform, so the server
// goroutine reports the error and runService returns. Occupying a real port
// does not work: Windows happily grants a second bind to the same address, and
// the server then blocks in Accept forever.
func TestRunService_AppliesLogLevelDuringStartup(t *testing.T) {
	withDefaultLogger(t)

	opts := &natsserver.Options{
		Host:      "127.0.0.1",
		Port:      -1, // random free port
		JetStream: true,
		StoreDir:  t.TempDir(),
	}
	ns, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatalf("create embedded nats: %v", err)
	}
	ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		t.Fatal("embedded nats not ready")
	}
	t.Cleanup(ns.Shutdown)

	t.Setenv("NATS_URL", ns.ClientURL())
	t.Setenv("NEPHTYS_PORT", "99999") // Out of range: net.Listen rejects it everywhere.
	t.Setenv("NEPHTYS_LOG_LEVEL", "error")

	errCh := make(chan error, 1)
	// Startup logs at info and below must be suppressed by the level we set,
	// so capture whatever does escape and assert on it afterwards.
	out := captureStderr(t, func() {
		errCh <- runService()
	})

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("runService returned nil despite an unavailable REST port")
		}
		if !strings.Contains(err.Error(), "server error") {
			t.Errorf("error %q does not identify the server as the failure", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runService did not return after the listen failure")
	}

	// The banner is logged at info; at error level it must not appear. This is
	// the actual regression guard: before the fix the level was ignored.
	if strings.Contains(out, "Starting Nephtys Edge Connector") {
		t.Errorf("NEPHTYS_LOG_LEVEL=error did not suppress the info banner: %q", out)
	}
	if !slog.Default().Enabled(context.Background(), slog.LevelError) {
		t.Error("error level should remain enabled")
	}
	if slog.Default().Enabled(context.Background(), slog.LevelInfo) {
		t.Error("NEPHTYS_LOG_LEVEL=error should suppress info")
	}
}
