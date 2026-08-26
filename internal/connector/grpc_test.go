package connector

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"nephtys/internal/domain"
	pb "nephtys/internal/grpc/streamer"
)

func TestGrpcSource_IDAndDefaults(t *testing.T) {
	src := NewGrpcSource("grpc-id-test", "topic", nil)
	if src.ID() != "grpc-id-test" {
		t.Errorf("expected grpc-id-test, got %s", src.ID())
	}
	if src.config.Port != "50051" {
		t.Errorf("expected default port 50051, got %s", src.config.Port)
	}
	if src.Addr() != nil {
		t.Errorf("a source that has not been opened reports address %v", src.Addr())
	}
}

// TestGrpcSource_OpenReportsBindFailure is the connector half of the
// registration contract: a port that is already taken has to fail in Open,
// synchronously, so the caller that registered the stream is the one told.
func TestGrpcSource_OpenReportsBindFailure(t *testing.T) {
	// Bind the wildcard address, the same one the source binds. Holding only
	// 127.0.0.1:P does not conflict with 0.0.0.0:P on every platform, so a
	// loopback holder would make this test pass for the wrong reason.
	holder, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("bind holder: %v", err)
	}
	defer func() { _ = holder.Close() }()

	_, port, err := net.SplitHostPort(holder.Addr().String())
	if err != nil {
		t.Fatalf("split holder addr: %v", err)
	}

	src := NewGrpcSource("taken", "topic", &domain.GrpcConfig{Port: port})
	err = src.Open(context.Background())
	if err == nil {
		src.Close()
		t.Fatal("Open on a port held by another listener returned nil")
	}
	if !strings.Contains(err.Error(), port) {
		t.Errorf("Open error %q does not name the port %s", err, port)
	}
}

// TestGrpcSource_RunWithoutOpen guards the ordering the manager relies on.
func TestGrpcSource_RunWithoutOpen(t *testing.T) {
	src := NewGrpcSource("no-open", "topic", nil)
	err := src.Run(context.Background(), func(string, domain.StreamEvent) error { return nil }, func() {})
	if err == nil {
		t.Fatal("Run without Open returned nil")
	}
}

func TestGrpcSource_StreamEvents(t *testing.T) {
	// Port 0: the OS picks a free one, so a busy port cannot flake the test.
	src := NewGrpcSource("test-grpc-stream", "test-topic", &domain.GrpcConfig{Port: "0"})
	if err := src.Open(context.Background()); err != nil {
		t.Fatalf("open: %v", err)
	}
	defer src.Close()

	// Channel to capture published events
	published := make(chan domain.StreamEvent, 10)
	publishFunc := func(topic string, ev domain.StreamEvent) error {
		if topic != "test-topic" {
			t.Errorf("expected topic test-topic, got %s", topic)
		}
		published <- ev
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Run the session in a goroutine and wait for it to report itself live.
	ready := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		errCh <- src.Run(ctx, publishFunc, func() { close(ready) })
	}()

	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("source never reported ready")
	}

	// Connect a gRPC client
	conn, err := grpc.NewClient(src.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	defer func() { _ = conn.Close() }()

	client := pb.NewStreamerClient(conn)

	stream, err := client.StreamEvents(context.Background())
	if err != nil {
		t.Fatalf("failed to open stream: %v", err)
	}

	// Send a few events
	events := []struct {
		Type    string
		Payload []byte
	}{
		{"type1", []byte(`{"key":"value1"}`)},
		{"type2", []byte(`{"key":"value2"}`)},
	}

	for _, ev := range events {
		req := &pb.IngestRequest{
			Type:    ev.Type,
			Payload: ev.Payload,
		}
		if err := stream.Send(req); err != nil {
			t.Fatalf("failed to send: %v", err)
		}
	}

	// Close stream and receive response
	resp, err := stream.CloseAndRecv()
	if err != nil {
		t.Fatalf("failed to close and recv: %v", err)
	}

	if resp.ProcessedCount != int64(len(events)) {
		t.Errorf("expected %d processed, got %d", len(events), resp.ProcessedCount)
	}

	// Verify events were published
	for ind, ev := range events {
		select {
		case pubEv := <-published:
			if pubEv.Type != ev.Type {
				t.Errorf("expected event type %s, got %s", ev.Type, pubEv.Type)
			}
			if string(pubEv.Payload) != string(ev.Payload) {
				t.Errorf("expected payload %s, got %s", ev.Payload, pubEv.Payload)
			}
		case <-time.After(time.Second):
			t.Fatalf("timeout waiting for event %d", ind)
		}
	}

	// Tell the source to stop
	cancel()

	// Wait for Run to return
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Run after cancellation returned %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}
