package connector

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"nephtys/internal/domain"
	pb "nephtys/internal/grpc/streamer"
)

func TestGrpcSource_IDAndStatus(t *testing.T) {
	src := NewGrpcSource("grpc-id-test", "topic", nil)
	if src.ID() != "grpc-id-test" {
		t.Errorf("expected grpc-id-test, got %s", src.ID())
	}
	if src.Status() != domain.StatusIdle {
		t.Errorf("expected idle, got %s", src.Status())
	}
}

func TestGrpcSource_StreamEvents(t *testing.T) {
	config := &domain.GrpcConfig{
		Port: "50055", // Hardcoded port for test
	}
	src := NewGrpcSource("test-grpc-stream", "test-topic", config)

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

	// Start the source in a goroutine
	errCh := make(chan error, 1)
	go func() {
		errCh <- src.Start(ctx, publishFunc)
	}()

	// Wait for the server to spin up
	time.Sleep(100 * time.Millisecond)

	// Connect a gRPC client
	conn, err := grpc.NewClient("localhost:50055", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	defer conn.Close()

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
	src.Stop()

	// Wait for Start to return
	select {
	case err := <-errCh:
		if err != nil && err != context.Canceled {
			t.Errorf("expected Canceled error, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Start did not return after Stop")
	}
}
