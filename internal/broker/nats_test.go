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
	return startTestBenchServer(t)
}

// startTestBenchServer is the testing.TB variant used by both tests and benchmarks.
func startTestBenchServer(tb testing.TB) *natsserver.Server {
	tb.Helper()
	opts := &natsserver.Options{
		Host:      "127.0.0.1",
		Port:      -1, // random port
		JetStream: true,
		StoreDir:  tb.TempDir(),
	}
	srv, err := natsserver.NewServer(opts)
	if err != nil {
		tb.Fatalf("failed to create test server: %v", err)
	}
	srv.Start()
	if !srv.ReadyForConnections(5 * time.Second) {
		tb.Fatal("nats server not ready")
	}
	tb.Cleanup(srv.Shutdown)
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

// A broker outage is only survivable if the client keeps trying: the NATS
// default is 60 attempts, after which the connection is closed for good and the
// process can never publish again. Nothing observable distinguishes the two
// policies inside a test of tolerable length — a bounded budget takes about two
// minutes to exhaust — so this asserts the option the recovery path depends on.
func TestConnect_ReconnectsIndefinitely(t *testing.T) {
	srv := startTestServer(t)

	brk, err := Connect(srv.ClientURL(), DefaultConfig())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer brk.Close()

	if got := brk.conn.Opts.MaxReconnect; got >= 0 {
		t.Errorf("expected an unlimited reconnect budget, got %d attempts", got)
	}
	if brk.conn.Opts.ReconnectJitter <= 0 {
		t.Error("expected reconnect jitter so instances do not retry in lockstep")
	}
}

// ConnState is served by an endpoint with no auth, so it must stay inside the
// client's own vocabulary rather than quoting a URL that can carry credentials.
func TestConnState(t *testing.T) {
	srv := startTestServer(t)

	brk, err := Connect(srv.ClientURL(), DefaultConfig())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	if got := brk.ConnState(); got != "CONNECTED" {
		t.Errorf("expected CONNECTED, got %q", got)
	}

	brk.Close()
	if got := brk.ConnState(); got == "CONNECTED" {
		t.Errorf("expected a closed connection to stop reporting CONNECTED, got %q", got)
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

func TestEnsureStream_UpdatesExistingStream(t *testing.T) {
	srv := startTestServer(t)

	brk, err := Connect(srv.ClientURL(), DefaultConfig())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer brk.Close()

	if err := brk.EnsureStream("TEST", []string{"test.old.>"}); err != nil {
		t.Fatalf("ensure stream initial: %v", err)
	}
	if err := brk.EnsureStream("TEST", []string{"test.new.>"}); err != nil {
		t.Fatalf("ensure stream update: %v", err)
	}

	info, err := brk.js.StreamInfo("TEST")
	if err != nil {
		t.Fatalf("stream info: %v", err)
	}
	if len(info.Config.Subjects) != 1 || info.Config.Subjects[0] != "test.new.>" {
		t.Fatalf("expected updated subjects, got %v", info.Config.Subjects)
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
	if msg.Header.Get("Content-Type") != domain.ContentTypeJSON {
		t.Errorf("expected JSON content type header, got %q", msg.Header.Get("Content-Type"))
	}
}

func TestPublish_BinaryPayload(t *testing.T) {
	srv := startTestServer(t)

	brk, err := Connect(srv.ClientURL(), DefaultConfig())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer brk.Close()

	if err := brk.EnsureStream("BINARY", []string{"bin.>"}); err != nil {
		t.Fatalf("ensure stream: %v", err)
	}

	event := domain.StreamEvent{
		Source:      "test-source",
		Type:        "arrow-batch",
		Timestamp:   time.Now().UnixMilli(),
		Seq:         42,
		ContentType: domain.ContentTypeArrowStream,
		Data:        []byte{0xff, 0x00, 0x01, 0x02},
	}

	if err := brk.Publish("bin.test", event); err != nil {
		t.Fatalf("publish: %v", err)
	}

	sub, err := brk.js.SubscribeSync("bin.test")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	msg, err := sub.NextMsg(2 * time.Second)
	if err != nil {
		t.Fatalf("next msg: %v", err)
	}

	if string(msg.Data) != string(event.Data) {
		t.Errorf("expected raw binary payload %v, got %v", event.Data, msg.Data)
	}
	if msg.Header.Get("Content-Type") != domain.ContentTypeArrowStream {
		t.Errorf("expected Arrow content type, got %q", msg.Header.Get("Content-Type"))
	}
	if msg.Header.Get("X-Nephtys-Seq") != "42" {
		t.Errorf("expected sequence header 42, got %q", msg.Header.Get("X-Nephtys-Seq"))
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
