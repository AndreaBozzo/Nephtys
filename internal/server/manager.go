package server

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"nephtys/internal/broker"
	"nephtys/internal/connector"
	"nephtys/internal/domain"
	"nephtys/internal/pipeline"
)

// StreamManager tracks running stream sources and manages their lifecycle.
type StreamManager struct {
	mu       sync.RWMutex
	sources  map[string]connector.StreamSource
	cancels  map[string]context.CancelFunc
	broker   *broker.Broker
	pipeline *pipeline.Pipeline
	logger   *slog.Logger
}

// NewStreamManager creates a manager backed by the given broker and pipeline.
func NewStreamManager(brk *broker.Broker, pipe *pipeline.Pipeline) *StreamManager {
	return &StreamManager{
		sources:  make(map[string]connector.StreamSource),
		cancels:  make(map[string]context.CancelFunc),
		broker:   brk,
		pipeline: pipe,
		logger:   slog.With("component", "manager"),
	}
}

// StreamInfo is the API representation of a running stream.
type StreamInfo struct {
	ID     string              `json:"id"`
	Status domain.SourceStatus `json:"status"`
}

// Register adds a source and starts it in a background goroutine.
func (m *StreamManager) Register(source connector.StreamSource) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	id := source.ID()
	if _, exists := m.sources[id]; exists {
		return fmt.Errorf("source %q already registered", id)
	}

	ctx, cancel := context.WithCancel(context.Background())
	m.sources[id] = source
	m.cancels[id] = cancel

	// The publish function wired through the pipeline
	publish := func(topic string, event domain.StreamEvent) error {
		processed, ok := m.pipeline.Process(event)
		if !ok {
			return nil // event dropped by pipeline
		}
		return m.broker.Publish(topic, processed)
	}

	go func() {
		if err := source.Start(ctx, publish); err != nil && ctx.Err() == nil {
			m.logger.Error("Source terminated with error", "id", id, "error", err)
		}
	}()

	m.logger.Info("Source registered and started", "id", id)
	return nil
}

// Remove stops and removes a source by ID.
func (m *StreamManager) Remove(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	source, exists := m.sources[id]
	if !exists {
		return fmt.Errorf("source %q not found", id)
	}

	source.Stop()
	if cancel, ok := m.cancels[id]; ok {
		cancel()
	}
	delete(m.sources, id)
	delete(m.cancels, id)

	m.logger.Info("Source removed", "id", id)
	return nil
}

// List returns info about all registered sources.
func (m *StreamManager) List() []StreamInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()

	infos := make([]StreamInfo, 0, len(m.sources))
	for _, src := range m.sources {
		infos = append(infos, StreamInfo{
			ID:     src.ID(),
			Status: src.Status(),
		})
	}
	return infos
}

// StopAll gracefully stops all registered sources.
func (m *StreamManager) StopAll() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for id, source := range m.sources {
		source.Stop()
		if cancel, ok := m.cancels[id]; ok {
			cancel()
		}
		m.logger.Info("Source stopped", "id", id)
	}
	m.sources = make(map[string]connector.StreamSource)
	m.cancels = make(map[string]context.CancelFunc)
}
