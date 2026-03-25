package broker

import (
	"encoding/json"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"nephtys/internal/domain"
)

// startTestServer starts an embedded NATS server with JetStream enabled.
func startTestServer(t *testing.T) *natsserver.Server {
	t.Helper()
	opts := &natsserver.Options{
		Host:      "127.0.0.1",
		Port:      -1, // random port
		JetStream: true,
		StoreDir:  t.TempDir(),
	}
	srv, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatalf("failed to create test server: %v", err)
	}
	srv.Start()
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats server not ready")
	}
	t.Cleanup(srv.Shutdown)
	return srv
}

func TestConnect_Success(t *testing.T) {
	srv := startTestServer(t)

	brk, err := Connect(srv.ClientURL(), DefaultConfig())
	if err != nil {
		t.Fatalf("expected successful connect, got: %v", err)
	}
	defer brk.Close()

	if !brk.IsConnected() {
		t.Error("expected broker to be connected")
	}
}

func TestConnect_BadURL(t *testing.T) {
	_, err := Connect("nats://127.0.0.1:1", DefaultConfig())
	if err == nil {
		t.Error("expected error for bad URL")
	}
}

func TestEnsureStream(t *testing.T) {
	srv := startTestServer(t)

	brk, err := Connect(srv.ClientURL(), DefaultConfig())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer brk.Close()

	if err := brk.EnsureStream("TEST", []string{"test.>"}); err != nil {
		t.Fatalf("ensure stream: %v", err)
	}

	// Verify stream exists via JetStream info
	info, err := brk.js.StreamInfo("TEST")
	if err != nil {
		t.Fatalf("stream info: %v", err)
	}
	if info.Config.Name != "TEST" {
		t.Errorf("expected stream name TEST, got %s", info.Config.Name)
	}
}

func TestPublish_RoundTrip(t *testing.T) {
	srv := startTestServer(t)

	brk, err := Connect(srv.ClientURL(), DefaultConfig())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer brk.Close()

	if err := brk.EnsureStream("ROUNDTRIP", []string{"rt.>"}); err != nil {
		t.Fatalf("ensure stream: %v", err)
	}

	event := domain.StreamEvent{
		Source:    "test-source",
		Type:      "test-event",
		Timestamp: time.Now().UnixMilli(),
		Payload:   json.RawMessage(`{"key":"value"}`),
	}

	if err := brk.Publish("rt.test", event); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// Subscribe and verify
	sub, err := brk.js.SubscribeSync("rt.test")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	msg, err := sub.NextMsg(2 * time.Second)
	if err != nil {
		t.Fatalf("next msg: %v", err)
	}

	var received domain.StreamEvent
	if err := json.Unmarshal(msg.Data, &received); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if received.Source != "test-source" {
		t.Errorf("expected source test-source, got %s", received.Source)
	}
	if received.Type != "test-event" {
		t.Errorf("expected type test-event, got %s", received.Type)
	}
}

func TestClose(t *testing.T) {
	srv := startTestServer(t)

	brk, err := Connect(srv.ClientURL(), DefaultConfig())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	brk.Close()

	// After drain+close, the connection should no longer be connected
	if brk.conn.Status() == nats.CONNECTED {
		t.Error("expected connection to not be CONNECTED after Close")
	}
}
