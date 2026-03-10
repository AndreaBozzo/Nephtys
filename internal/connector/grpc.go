package connector

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"google.golang.org/grpc"

	"nephtys/internal/domain"
	pb "nephtys/internal/grpc/streamer"
)

// GrpcSource runs a gRPC server to ingest events via client-streaming.
type GrpcSource struct {
	pb.UnimplementedStreamerServer

	id     string
	topic  string
	config *domain.GrpcConfig
	logger *slog.Logger

	server *grpc.Server

	mu     sync.RWMutex
	status domain.SourceStatus
	cancel context.CancelFunc

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
		status: domain.StatusIdle,
		logger: slog.With("connector", id, "kind", "grpc"),
	}
}

func (g *GrpcSource) ID() string { return g.id }

func (g *GrpcSource) Status() domain.SourceStatus {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.status
}

func (g *GrpcSource) setStatus(s domain.SourceStatus) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.status = s
}

// Start boots the gRPC server and processes incoming streams.
func (g *GrpcSource) Start(ctx context.Context, publish PublishFunc) error {
	ctx, g.cancel = context.WithCancel(ctx)
	g.publish = publish

	lis, err := net.Listen("tcp", ":"+g.config.Port)
	if err != nil {
		g.setStatus(domain.StatusError)
		return fmt.Errorf("failed to listen on port %s: %w", g.config.Port, err)
	}

	g.server = grpc.NewServer()
	pb.RegisterStreamerServer(g.server, g)

	g.setStatus(domain.StatusRunning)
	g.logger.Info("Starting gRPC Streamer server", "port", g.config.Port)

	errChan := make(chan error, 1)
	go func() {
		if err := g.server.Serve(lis); err != nil {
			g.logger.Error("gRPC server failed", "error", err)
			g.setStatus(domain.StatusError)
			errChan <- err
		}
	}()

	for {
		select {
		case err := <-errChan:
			return err
		case <-ctx.Done():
			g.setStatus(domain.StatusStopped)
			g.logger.Info("Stopping gRPC Streamer server")
			g.server.GracefulStop()
			return ctx.Err()
		}
	}
}

// Stop shuts down the gRPC server.
func (g *GrpcSource) Stop() {
	if g.cancel != nil {
		g.cancel()
	}
}

// StreamEvents implements the Streamer server method for client-streaming.
func (g *GrpcSource) StreamEvents(stream pb.Streamer_StreamEventsServer) error {
	var processedCount int64

	for {
		req, err := stream.Recv()
		if err != nil {
			// io.EOF is returned when the client closes the stream gracefully.
			if err.Error() == "EOF" {
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
