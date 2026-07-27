package server

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"nephtys/internal/broker"
	"nephtys/internal/connector"
	"nephtys/internal/domain"
	"nephtys/internal/pipeline"
	"nephtys/internal/store"
	"nephtys/internal/telemetry"
)

// StreamManager tracks running stream sources and manages their lifecycle.
// It persists stream configurations to a JetStream KV store so streams
// survive restarts.
type StreamManager struct {
	mu              sync.RWMutex
	sources         map[string]connector.StreamSource
	pipelineRefs    map[string]*atomic.Pointer[pipeline.Handler]
	pipelineCancels map[string]context.CancelFunc // cancels batch goroutines on pipeline swap
	cancels         map[string]context.CancelFunc
	dones           map[string]chan struct{}
	stateDones      map[string]chan struct{}
	runtimes        map[string]*streamRuntime
	broker          *broker.Broker
	store           *store.StreamStore // nil in tests
	logger          *slog.Logger
}

// NewStreamManager creates a manager backed by the given broker and store.
// The store may be nil (e.g. in unit tests), in which case persistence is disabled.
func NewStreamManager(brk *broker.Broker, st *store.StreamStore) *StreamManager {
	return &StreamManager{
		sources:         make(map[string]connector.StreamSource),
		pipelineRefs:    make(map[string]*atomic.Pointer[pipeline.Handler]),
		pipelineCancels: make(map[string]context.CancelFunc),
		cancels:         make(map[string]context.CancelFunc),
		dones:           make(map[string]chan struct{}),
		stateDones:      make(map[string]chan struct{}),
		runtimes:        make(map[string]*streamRuntime),
		broker:          brk,
		store:           st,
		logger:          slog.With("component", "manager"),
	}
}

// StreamInfo is the API representation of a running stream.
type StreamInfo struct {
	ID            string              `json:"id"`
	Status        domain.SourceStatus `json:"status"`
	Health        string              `json:"health"`
	LastMessageAt *time.Time          `json:"last_message_at,omitempty"`
}

type streamRuntime struct {
	lastMessageUnixNano atomic.Int64
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

	m.startSourceLocked(id, source, cfg)
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
	// Cancel pipeline context (stops batch goroutines)
	if pipeCancel, ok := m.pipelineCancels[id]; ok {
		pipeCancel()
	}

	// Wait for the source's goroutine to finish shutting down
	if done, ok := m.dones[id]; ok {
		<-done
	}
	if stateDone, ok := m.stateDones[id]; ok {
		<-stateDone
	}

	delete(m.sources, id)
	delete(m.pipelineRefs, id)
	delete(m.pipelineCancels, id)
	delete(m.cancels, id)
	delete(m.dones, id)
	delete(m.stateDones, id)
	delete(m.runtimes, id)
	telemetry.DeleteStreamSeries(id)

	// Remove persisted config
	if m.store != nil {
		if err := m.store.Delete(id); err != nil {
			m.logger.Warn("Failed to delete persisted config", "id", id, "error", err)
		}
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
		m.startSourceLocked(cfg.ID, source, cfg)
		m.logger.Info("Stream restored", "id", cfg.ID, "kind", cfg.Kind)
	}

	m.logger.Info("Restore complete", "count", len(configs))
	return nil
}

// UpdatePipeline hot-swaps the pipeline config for a running stream.
func (m *StreamManager) UpdatePipeline(id string, pipelineCfg *domain.PipelineConfig) error {
	m.mu.RLock()
	_, exists := m.pipelineRefs[id]
	m.mu.RUnlock()
	if !exists {
		return fmt.Errorf("source %q not found", id)
	}

	// Build the replacement generation *before* retiring the running one.
	// The old handler has to stay usable right up to the swap: a batch
	// middleware refuses events once its context is cancelled, so cancelling
	// first would turn every event arriving during the rebuild into a
	// "context canceled" error handed back to the source.
	//
	// The build happens outside the lock. It starts middleware goroutines and
	// touches Prometheus collectors, and no other manager operation should
	// queue behind that.
	pipeCtx, pipeCancel := context.WithCancel(context.Background())
	pipe := pipeline.BuildFromConfig(pipeCtx, id, pipelineCfg)
	handler := pipe.Execute(func(topic string, event domain.StreamEvent) error {
		telemetry.EventsPublished.WithLabelValues(id).Inc()
		telemetry.BytesPublished.WithLabelValues(id).Add(float64(eventPayloadSize(event)))
		return m.broker.Publish(topic, event)
	})

	m.mu.Lock()
	atomicRef, exists := m.pipelineRefs[id]
	if !exists {
		// The stream was removed while we were building. Retire what we just
		// built rather than installing it on a dead stream, and drop the
		// dedup series it published — Remove already cleaned those up, so
		// leaving them would resurrect gauges for a stream that is gone.
		m.mu.Unlock()
		pipeCancel()
		telemetry.DeleteDedupSeries(id)
		return fmt.Errorf("source %q not found", id)
	}

	oldCancel := m.pipelineCancels[id]
	m.pipelineCancels[id] = pipeCancel
	atomicRef.Store(&handler)
	m.mu.Unlock()

	// Retire the previous generation only after the swap. Its batch worker
	// drains and flushes whatever it had already accepted (see NewBatch), so
	// the handover neither drops buffered events nor rejects new ones.
	//
	// One narrow window survives: a publisher that has already loaded the old
	// handler pointer but not yet called it can still observe the cancelled
	// context. Closing it would require refcounting in-flight publishers on
	// the hot path, which the atomic-pointer design exists to avoid.
	if oldCancel != nil {
		oldCancel()
	}
	m.logger.Info("Stream pipeline hot-reloaded", "id", id)

	// The swap is runtime-only: the JetStream KV copy of the config still
	// describes the previous pipeline, so a restart silently reverts it.
	// That contradicts the documented persistence contract and is tracked
	// as a defect, not a design choice — see #28.
	return nil
}

// List returns info about all registered sources.
func (m *StreamManager) List() []StreamInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()

	infos := make([]StreamInfo, 0, len(m.sources))
	for _, src := range m.sources {
		runtime := m.runtimes[src.ID()]
		status := src.Status()
		infos = append(infos, StreamInfo{
			ID:            src.ID(),
			Status:        status,
			Health:        sourceHealth(status),
			LastMessageAt: runtimeLastMessageAt(runtime),
		})
	}
	return infos
}

// StopAll gracefully stops all registered sources.
func (m *StreamManager) StopAll() {
	m.mu.Lock()
	defer m.mu.Unlock()

	var wg sync.WaitGroup

	for id, source := range m.sources {
		source.Stop()
		if cancel, ok := m.cancels[id]; ok {
			cancel()
		}
		// Cancel pipeline context (stops batch goroutines)
		if pipeCancel, ok := m.pipelineCancels[id]; ok {
			pipeCancel()
		}

		if done, ok := m.dones[id]; ok {
			wg.Add(1)
			go func(d chan struct{}) {
				defer wg.Done()
				<-d
			}(done)
		}
		if stateDone, ok := m.stateDones[id]; ok {
			wg.Add(1)
			go func(d chan struct{}) {
				defer wg.Done()
				<-d
			}(stateDone)
		}
		m.logger.Info("Source stopped", "id", id)
	}

	// Wait for all sources to cleanly exit
	wg.Wait()
	for id := range m.sources {
		telemetry.DeleteStreamState(id)
	}

	m.sources = make(map[string]connector.StreamSource)
	m.pipelineRefs = make(map[string]*atomic.Pointer[pipeline.Handler])
	m.pipelineCancels = make(map[string]context.CancelFunc)
	m.cancels = make(map[string]context.CancelFunc)
	m.dones = make(map[string]chan struct{})
	m.stateDones = make(map[string]chan struct{})
	m.runtimes = make(map[string]*streamRuntime)
}

// startSourceLocked launches a source in a goroutine. Must be called with mu held.
func (m *StreamManager) startSourceLocked(id string, source connector.StreamSource, cfg domain.StreamSourceConfig) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	stateDone := make(chan struct{})

	// Pipeline gets its own context so batch goroutines can be cancelled independently
	pipeCtx, pipeCancel := context.WithCancel(context.Background())

	m.sources[id] = source
	m.cancels[id] = cancel
	m.pipelineCancels[id] = pipeCancel
	m.dones[id] = done
	m.stateDones[id] = stateDone
	runtime := &streamRuntime{}
	m.runtimes[id] = runtime

	// Build initial handler
	pipe := pipeline.BuildFromConfig(pipeCtx, id, cfg.Pipeline)
	handler := pipe.Execute(func(topic string, event domain.StreamEvent) error {
		telemetry.EventsPublished.WithLabelValues(id).Inc()
		telemetry.BytesPublished.WithLabelValues(id).Add(float64(eventPayloadSize(event)))
		return m.broker.Publish(topic, event)
	})

	var atomicRef atomic.Pointer[pipeline.Handler]
	atomicRef.Store(&handler)
	m.pipelineRefs[id] = &atomicRef

	instrumentedPublish := func(topic string, event domain.StreamEvent) error {
		start := time.Now()
		runtime.lastMessageUnixNano.Store(start.UTC().UnixNano())
		telemetry.EventsIngested.WithLabelValues(id).Inc()
		telemetry.BytesIngested.WithLabelValues(id).Add(float64(eventPayloadSize(event)))

		h := *atomicRef.Load()
		err := h(topic, event)
		telemetry.EventProcessingDuration.WithLabelValues(id).Observe(time.Since(start).Seconds())
		return err
	}

	go func() {
		defer close(done)
		if err := source.Start(ctx, connector.PublishFunc(instrumentedPublish)); err != nil && ctx.Err() == nil {
			m.logger.Error("Source terminated with error", "id", id, "error", err)
		}
	}()

	go func() {
		defer close(stateDone)
		trackSourceState(ctx, id, source)
	}()

	m.logger.Info("Source registered and started", "id", id)
}

func trackSourceState(ctx context.Context, id string, source connector.StreamSource) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	trackSourceStateOnTicks(ctx, id, source, ticker.C)
}

func trackSourceStateOnTicks(ctx context.Context, id string, source connector.StreamSource, ticks <-chan time.Time) {
	update := func() {
		telemetry.SetStreamState(id, metricState(source.Status()))
	}
	update()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			update()
		}
	}
}

func sourceHealth(status domain.SourceStatus) string {
	switch status {
	case domain.StatusRunning:
		return "healthy"
	case domain.StatusError:
		return "errored"
	default:
		return "degraded"
	}
}

func metricState(status domain.SourceStatus) string {
	switch status {
	case domain.StatusRunning:
		return "connected"
	case domain.StatusError:
		return "errored"
	case domain.StatusStopped:
		return "stopped"
	default:
		return "reconnecting"
	}
}

func runtimeLastMessageAt(runtime *streamRuntime) *time.Time {
	if runtime == nil {
		return nil
	}
	nanos := runtime.lastMessageUnixNano.Load()
	if nanos == 0 {
		return nil
	}
	lastMessageAt := time.Unix(0, nanos).UTC()
	return &lastMessageAt
}

func eventPayloadSize(event domain.StreamEvent) int {
	if len(event.Data) > 0 {
		return len(event.Data)
	}
	return len(event.Payload)
}

// sourceFromConfig creates a StreamSource from a persisted config.
func sourceFromConfig(cfg domain.StreamSourceConfig) (connector.StreamSource, error) {
	switch cfg.Kind {
	case "websocket":
		return connector.NewWebSocketSource(cfg.ID, cfg.URL, cfg.Topic, cfg.Websocket), nil
	case "rest_poller":
		return connector.NewRESTPollerSource(cfg.ID, cfg.URL, cfg.Topic, cfg.RestPoller), nil
	case "webhook":
		return connector.NewWebhookSource(cfg.ID, cfg.Topic, cfg.Webhook), nil
	case "grpc":
		return connector.NewGrpcSource(cfg.ID, cfg.Topic, cfg.Grpc), nil
	case "sse":
		return connector.NewSSESource(cfg.ID, cfg.URL, cfg.Topic, cfg.Sse), nil
	default:
		return nil, fmt.Errorf("unsupported kind: %s", cfg.Kind)
	}
}
