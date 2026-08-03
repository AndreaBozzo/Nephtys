package server

import (
	"context"
	"errors"
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

// configStore is the persistence surface the manager depends on.
// store.StreamStore is the only implementation; the indirection exists so the
// paths that have to behave correctly when persistence *fails* can be tested
// without breaking a live JetStream.
type configStore interface {
	Put(cfg domain.StreamSourceConfig) error
	Delete(id string) error
	List() ([]domain.StreamSourceConfig, error)
}

// compile-time assertion that the production store satisfies the interface the
// manager declares.
var _ configStore = (*store.StreamStore)(nil)

// StreamManager tracks running stream sources and manages their lifecycle.
// It persists stream configurations to a JetStream KV store so streams
// survive restarts.
type StreamManager struct {
	mu sync.RWMutex

	sources map[string]connector.StreamSource

	// generations holds the pipeline generation each stream is currently
	// publishing through. The pointer is swapped on a hot-swap; the generation
	// it displaces is retired, which is what makes the handover lossless.
	generations map[string]*atomic.Pointer[pipeline.Generation]

	// configs holds the *effective* configuration of each running stream: the
	// one it was registered with, amended by every accepted pipeline update.
	// It is what gets persisted, so it has to track the running generation
	// rather than the registration-time config.
	configs map[string]domain.StreamSourceConfig

	cancels    map[string]context.CancelFunc
	dones      map[string]chan struct{}
	stateDones map[string]chan struct{}
	runtimes   map[string]*streamRuntime
	broker     *broker.Broker
	store      configStore // nil in tests
	logger     *slog.Logger
}

// NewStreamManager creates a manager backed by the given broker and store.
// The store may be nil (e.g. in unit tests), in which case persistence is disabled.
func NewStreamManager(brk *broker.Broker, st configStore) *StreamManager {
	return &StreamManager{
		sources:     make(map[string]connector.StreamSource),
		generations: make(map[string]*atomic.Pointer[pipeline.Generation]),
		configs:     make(map[string]domain.StreamSourceConfig),
		cancels:     make(map[string]context.CancelFunc),
		dones:       make(map[string]chan struct{}),
		stateDones:  make(map[string]chan struct{}),
		runtimes:    make(map[string]*streamRuntime),
		broker:      brk,
		store:       st,
		logger:      slog.With("component", "manager"),
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
		return fmt.Errorf("source %q: %w", id, ErrStreamExists)
	}

	// Persist config for restart recovery
	if m.store != nil {
		if err := m.store.Put(cfg); err != nil {
			return fmt.Errorf("persist config: %w", err)
		}
	}

	if err := m.startSourceLocked(id, source, cfg); err != nil {
		// Nothing was started, so leave no persisted config behind claiming
		// otherwise.
		if m.store != nil {
			if delErr := m.store.Delete(id); delErr != nil {
				m.logger.Warn("Failed to delete config for unstartable stream", "id", id, "error", delErr)
			}
		}
		return err
	}
	return nil
}

// Remove stops and removes a source by ID, also deleting its persisted config.
func (m *StreamManager) Remove(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	source, exists := m.sources[id]
	if !exists {
		return fmt.Errorf("source %q: %w", id, ErrStreamNotFound)
	}

	// Drop the persisted config before tearing anything down. A stream that is
	// stopped while its config survives comes back on the next restart — the
	// same runtime/stored divergence UpdatePipeline avoids by persisting before
	// it swaps, in the other direction. Nothing has been stopped at this point,
	// so a store that refuses the delete leaves the stream running and the
	// caller told why, rather than leaving a removal that silently un-removes
	// itself later.
	if m.store != nil {
		if err := m.store.Delete(id); err != nil {
			return fmt.Errorf("delete persisted config for %q: %w", id, err)
		}
	}

	source.Stop()
	if cancel, ok := m.cancels[id]; ok {
		cancel()
	}

	// Wait for the source's goroutine to finish shutting down
	if done, ok := m.dones[id]; ok {
		<-done
	}
	if stateDone, ok := m.stateDones[id]; ok {
		<-stateDone
	}

	// Retire only once the source has stopped publishing, so the pipeline's
	// final flush is the last thing that happens on this stream. Retire blocks
	// until every event the pipeline accepted has been handed to the broker.
	// sources and generations are populated and deleted together, so the entry
	// is present here.
	m.generations[id].Load().Retire()

	delete(m.sources, id)
	delete(m.generations, id)
	delete(m.configs, id)
	delete(m.cancels, id)
	delete(m.dones, id)
	delete(m.stateDones, id)
	delete(m.runtimes, id)
	telemetry.DeleteStreamSeries(id)

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

	restored := 0
	for _, cfg := range configs {
		// Re-validate on the way in. The KV bucket holds whatever the version
		// that wrote it accepted, so without this check restore is the one path
		// that can start a stream from configuration the current validator
		// rejects — the config contract would hold for new streams and quietly
		// not hold for surviving ones.
		if err := validateStreamConfig(cfg); err != nil {
			m.logger.Warn("Skipping invalid persisted stream", "id", cfg.ID, "error", err)
			continue
		}
		source, err := sourceFromConfig(cfg)
		if err != nil {
			m.logger.Warn("Skipping unrestorable stream", "id", cfg.ID, "error", err)
			continue
		}
		if err := m.startSourceLocked(cfg.ID, source, cfg); err != nil {
			m.logger.Warn("Skipping unstartable stream", "id", cfg.ID, "error", err)
			continue
		}
		restored++
		m.logger.Info("Stream restored", "id", cfg.ID, "kind", cfg.Kind)
	}

	m.logger.Info("Restore complete", "restored", restored, "persisted", len(configs))
	return nil
}

// Errors the manager returns for conditions the REST layer maps to a specific
// status. Anything else is a failure to apply an otherwise valid request — a
// dependency problem rather than the caller's fault — and maps to 503.
var (
	ErrStreamNotFound = errors.New("stream not found")
	ErrStreamExists   = errors.New("stream already registered")
)

// UpdatePipeline hot-swaps the pipeline config for a running stream and makes
// the change durable.
//
// Persistence is not an afterthought here: the stored config is updated before
// the swap, under the lock that also guards registration and removal, so the
// running pipeline and the stored one cannot disagree. A store that rejects the
// write leaves the stream on the pipeline the store still describes, and the
// caller is told the update did not take. The alternative ordering — swap, then
// persist — would answer 200 for a change a restart silently reverts, which is
// the defect this replaces.
func (m *StreamManager) UpdatePipeline(id string, pipelineCfg *domain.PipelineConfig) error {
	m.mu.RLock()
	_, exists := m.generations[id]
	m.mu.RUnlock()
	if !exists {
		return fmt.Errorf("source %q: %w", id, ErrStreamNotFound)
	}

	// Build the replacement generation *before* retiring the running one.
	// The old generation has to stay usable right up to the swap: a retired
	// batch middleware stops buffering, so retiring first would push every
	// event arriving during the rebuild down the unbatched path.
	//
	// The build happens outside the lock. It starts middleware goroutines and
	// touches Prometheus collectors, and no other manager operation should
	// queue behind that.
	next, err := pipeline.NewGeneration(id, pipelineCfg, m.publishFunc(id))
	if err != nil {
		// The running generation is untouched: nothing has been swapped yet, so
		// a rejected update leaves the stream on its previous pipeline.
		return fmt.Errorf("build pipeline: %w", err)
	}

	m.mu.Lock()
	ref, exists := m.generations[id]
	if !exists {
		// The stream was removed while we were building. Retire what we just
		// built rather than installing it on a dead stream, and drop the
		// dedup series it published — Remove already cleaned those up, so
		// leaving them would resurrect gauges for a stream that is gone.
		m.mu.Unlock()
		next.Retire()
		telemetry.DeleteDedupSeries(id)
		return fmt.Errorf("source %q: %w", id, ErrStreamNotFound)
	}

	// The effective config is the registered one with its pipeline replaced.
	// Everything else about the stream — kind, url, topic, connector block — is
	// untouched by this endpoint and must survive the update.
	updated := m.configs[id]
	updated.Pipeline = pipelineCfg

	if m.store != nil {
		if err := m.store.Put(updated); err != nil {
			// Nothing has been swapped, so the stream keeps running the
			// pipeline the store still holds. Retire the replacement rather
			// than leaking its worker goroutines and buffered events.
			m.mu.Unlock()
			next.Retire()
			m.logger.Error("Pipeline update rejected: config not persisted", "id", id, "error", err)
			return fmt.Errorf("persist pipeline for %q: %w", id, err)
		}
	}

	m.configs[id] = updated
	previous := ref.Swap(next)
	m.mu.Unlock()

	// Retire the previous generation only after the swap, and outside the lock:
	// Retire waits for in-flight publishers and for the final flush, neither of
	// which any other manager operation should queue behind.
	//
	// Publishers that loaded the previous generation before the swap are the
	// reason this is a handshake rather than a cancel. Retire holds them off
	// from the buffers, waits for the ones already inside to leave, and only
	// then lets the batch worker drain — so an event accepted by the outgoing
	// generation is flushed by it rather than stranded in a buffer nobody owns.
	previous.Retire()
	m.logger.Info("Stream pipeline hot-reloaded", "id", id, "persisted", m.store != nil)
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

	// Retire each pipeline only after its source has stopped, and wait for the
	// final flush: on shutdown this is what stands between a buffered batch and
	// the process exiting without publishing it.
	for id := range m.sources {
		m.generations[id].Load().Retire()
		telemetry.DeleteStreamState(id)
	}

	m.sources = make(map[string]connector.StreamSource)
	m.generations = make(map[string]*atomic.Pointer[pipeline.Generation])
	m.configs = make(map[string]domain.StreamSourceConfig)
	m.cancels = make(map[string]context.CancelFunc)
	m.dones = make(map[string]chan struct{})
	m.stateDones = make(map[string]chan struct{})
	m.runtimes = make(map[string]*streamRuntime)
}

// publishFunc returns the terminal handler of a stream's pipeline: the step
// that counts the event as published and hands it to the broker. Every
// generation built for a stream — the first and each hot-swapped replacement —
// ends in this same function.
func (m *StreamManager) publishFunc(id string) pipeline.Handler {
	return func(topic string, event domain.StreamEvent) error {
		telemetry.EventsPublished.WithLabelValues(id).Inc()
		telemetry.BytesPublished.WithLabelValues(id).Add(float64(eventPayloadSize(event)))
		return m.broker.Publish(topic, event)
	}
}

// startSourceLocked launches a source in a goroutine. Must be called with mu held.
// It fails without registering anything if the stream's pipeline cannot be
// built, so a stream never runs with a pipeline other than the one configured.
func (m *StreamManager) startSourceLocked(id string, source connector.StreamSource, cfg domain.StreamSourceConfig) error {
	gen, err := pipeline.NewGeneration(id, cfg.Pipeline, m.publishFunc(id))
	if err != nil {
		return fmt.Errorf("build pipeline: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	stateDone := make(chan struct{})

	m.sources[id] = source
	m.configs[id] = cfg
	m.cancels[id] = cancel
	m.dones[id] = done
	m.stateDones[id] = stateDone
	runtime := &streamRuntime{}
	m.runtimes[id] = runtime

	var ref atomic.Pointer[pipeline.Generation]
	ref.Store(gen)
	m.generations[id] = &ref

	instrumentedPublish := func(topic string, event domain.StreamEvent) error {
		start := time.Now()
		runtime.lastMessageUnixNano.Store(start.UTC().UnixNano())
		telemetry.EventsIngested.WithLabelValues(id).Inc()
		telemetry.BytesIngested.WithLabelValues(id).Add(float64(eventPayloadSize(event)))

		err := ref.Load().Publish(topic, event)
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
	return nil
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
