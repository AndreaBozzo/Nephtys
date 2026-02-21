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
	"nephtys/internal/store"
)

// StreamManager tracks running stream sources and manages their lifecycle.
// It persists stream configurations to a JetStream KV store so streams
// survive restarts.
type StreamManager struct {
	mu       sync.RWMutex
	sources  map[string]connector.StreamSource
	cancels  map[string]context.CancelFunc
	broker   *broker.Broker
	pipeline *pipeline.Pipeline
	store    *store.StreamStore // nil in tests
	logger   *slog.Logger
}

// NewStreamManager creates a manager backed by the given broker, pipeline, and store.
// The store may be nil (e.g. in unit tests), in which case persistence is disabled.
func NewStreamManager(brk *broker.Broker, pipe *pipeline.Pipeline, st *store.StreamStore) *StreamManager {
	return &StreamManager{
		sources:  make(map[string]connector.StreamSource),
		cancels:  make(map[string]context.CancelFunc),
		broker:   brk,
		pipeline: pipe,
		store:    st,
		logger:   slog.With("component", "manager"),
	}
}

// StreamInfo is the API representation of a running stream.
type StreamInfo struct {
	ID     string              `json:"id"`
	Status domain.SourceStatus `json:"status"`
}

// Register adds a source and starts it in a background goroutine.
// If a store is configured, the stream config is persisted for auto-restore.
func (m *StreamManager) Register(source connector.StreamSource, cfg domain.StreamSourceConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	id := source.ID()
	if _, exists := m.sources[id]; exists {
		return fmt.Errorf("source %q already registered", id)
	}

	// Persist config for restart recovery
	if m.store != nil {
		if err := m.store.Put(cfg); err != nil {
			return fmt.Errorf("persist config: %w", err)
		}
	}

	m.startSourceLocked(id, source)
	return nil
}

// Remove stops and removes a source by ID, also deleting its persisted config.
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

	// Remove persisted config
	if m.store != nil {
		m.store.Delete(id)
	}

	m.logger.Info("Source removed", "id", id)
	return nil
}

// Restore loads persisted stream configs from the store and re-registers them.
// Called once on startup to resume streams from a previous run.
func (m *StreamManager) Restore() error {
	if m.store == nil {
		return nil
	}

	configs, err := m.store.List()
	if err != nil {
		return fmt.Errorf("load persisted configs: %w", err)
	}

	if len(configs) == 0 {
		m.logger.Info("No persisted streams to restore")
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	for _, cfg := range configs {
		source, err := sourceFromConfig(cfg)
		if err != nil {
			m.logger.Warn("Skipping unrestorable stream", "id", cfg.ID, "error", err)
			continue
		}
		m.startSourceLocked(cfg.ID, source)
		m.logger.Info("Stream restored", "id", cfg.ID, "kind", cfg.Kind)
	}

	m.logger.Info("Restore complete", "count", len(configs))
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

// startSourceLocked launches a source in a goroutine. Must be called with mu held.
func (m *StreamManager) startSourceLocked(id string, source connector.StreamSource) {
	ctx, cancel := context.WithCancel(context.Background())
	m.sources[id] = source
	m.cancels[id] = cancel

	publish := func(topic string, event domain.StreamEvent) error {
		processed, ok := m.pipeline.Process(event)
		if !ok {
			return nil
		}
		return m.broker.Publish(topic, processed)
	}

	go func() {
		if err := source.Start(ctx, publish); err != nil && ctx.Err() == nil {
			m.logger.Error("Source terminated with error", "id", id, "error", err)
		}
	}()

	m.logger.Info("Source registered and started", "id", id)
}

// sourceFromConfig creates a StreamSource from a persisted config.
func sourceFromConfig(cfg domain.StreamSourceConfig) (connector.StreamSource, error) {
	switch cfg.Kind {
	case "websocket":
		return connector.NewWebSocketSource(cfg.ID, cfg.URL, cfg.Topic), nil
	default:
		return nil, fmt.Errorf("unsupported kind: %s", cfg.Kind)
	}
}
