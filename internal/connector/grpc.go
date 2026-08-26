package connector

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"time"

	"google.golang.org/grpc"

	"nephtys/internal/domain"
	pb "nephtys/internal/grpc/streamer"
)

// GrpcSource runs a gRPC server to ingest events via client-streaming.
//
// Reliability model: this is an inbound (push) source. Unlike pull connectors
// (websocket, sse, rest_poller) which reconnect on transient failures, the gRPC
// source does not "reconnect" — it accepts whatever the upstream clients send.
// Stream resumption and retry on individual client streams are the client's
// responsibility.
//
// The listener is bound in Open, so a port conflict is reported to whoever
// registered the stream instead of being discovered by a goroutine after the
// API has already answered. Restarting a lost listener is the manager's
// decision, driven by the stream's restart policy.
type GrpcSource struct {
	pb.UnimplementedStreamerServer

	id     string
	topic  string
	config *domain.GrpcConfig
	logger *slog.Logger

	listener net.Listener
	server   *grpc.Server

	publish PublishFunc
}

// NewGrpcSource creates a new gRPC receiver connector.
func NewGrpcSource(id, topic string, config *domain.GrpcConfig) *GrpcSource {
	if config == nil {
		config = &domain.GrpcConfig{Port: "50051"}
	}
	if config.Port == "" {
		config.Port = "50051"
	}

	return &GrpcSource{
		id:     id,
		topic:  topic,
		config: config,
		logger: slog.With("connector", id, "kind", "grpc"),
	}
}

func (g *GrpcSource) ID() string { return g.id }

// Addr reports the address the listener is bound to. It is only meaningful
// between Open and Close, and exists so a test can bind port 0 and discover
// what it got.
func (g *GrpcSource) Addr() net.Addr {
	if g.listener == nil {
		return nil
	}
	return g.listener.Addr()
}

// Open binds the gRPC listener. A port already taken — by another stream or by
// any other process on the host — fails here, synchronously, with the address
// in the error.
func (g *GrpcSource) Open(ctx context.Context) error {
	var lc net.ListenConfig
	lis, err := lc.Listen(ctx, "tcp", ":"+g.config.Port)
	if err != nil {
		return fmt.Errorf("bind grpc listener on port %s: %w", g.config.Port, err)
	}
	g.listener = lis
	return nil
}

// Run serves incoming client streams until the server fails or ctx is cancelled.
func (g *GrpcSource) Run(ctx context.Context, publish PublishFunc, ready ReadyFunc) error {
	lis := g.listener
	if lis == nil {
		return errors.New("grpc source: Run called without a successful Open")
	}

	g.publish = publish
	srv := grpc.NewServer()
	pb.RegisterStreamerServer(srv, g)
	g.server = srv

	g.logger.Info("Serving gRPC Streamer", "port", g.config.Port)

	errChan := make(chan error, 1)
	go func() {
		// Serve the locals, not the fields: Close may clear the fields as soon
		// as Run returns, and this goroutine outlives neither.
		errChan <- srv.Serve(lis)
	}()

	// The listener is already bound, so the session is live the moment it is
	// being served.
	ready()

	select {
	case err := <-errChan:
		if err == nil {
			return nil
		}
		g.logger.Error("gRPC server failed", "error", err)
		return err
	case <-ctx.Done():
		g.logger.Info("Stopping gRPC Streamer server")
		srv.GracefulStop()
		// Wait for Serve to return, so no goroutine of this session survives it.
		<-errChan
		return nil
	}
}

// Close stops the server and releases the listener. Safe to call more than
// once, and safe on a source whose Run never started.
func (g *GrpcSource) Close() {
	if g.server != nil {
		g.server.Stop()
		g.server = nil
	}
	if g.listener != nil {
		// GracefulStop/Stop already closed it in the common path; this covers
		// an Open with no Run.
		_ = g.listener.Close()
		g.listener = nil
	}
}

// StreamEvents implements the Streamer server method for client-streaming.
func (g *GrpcSource) StreamEvents(stream pb.Streamer_StreamEventsServer) error {
	var processedCount int64

	for {
		req, err := stream.Recv()
		if err != nil {
			// io.EOF is returned when the client closes the stream gracefully.
			if errors.Is(err, io.EOF) {
				return stream.SendAndClose(&pb.IngestResponse{
					ProcessedCount: processedCount,
				})
			}
			g.logger.Error("Error receiving from stream", "error", err)
			return err
		}

		event := domain.StreamEvent{
			Source:    g.id,
			Type:      req.Type,
			Timestamp: time.Now().UnixMilli(),
			Payload:   req.Payload,
		}

		if err := g.publish(g.topic, event); err != nil {
			g.logger.Error("Publish failed", "error", err)
			return err
		}

		processedCount++
	}
}
