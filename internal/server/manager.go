package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
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

// admissionTimeout bounds source.Open. Open is local work by contract — a bind,
// a parse — so reaching this deadline is a bug rather than a slow network. It
// exists so such a bug cannot hold the manager lock forever.
const admissionTimeout = 5 * time.Second

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

	// generations holds the pipeline generation each stream is currently
	// publishing through. The pointer is swapped on a hot-swap; the generation
	// it displaces is retired, which is what makes the handover lossless.
	generations map[string]*atomic.Pointer[pipeline.Generation]

	// configs holds the *effective* configuration of each running stream: the
	// one it was registered with, amended by every accepted pipeline update.
	// It is what gets persisted, so it has to track the running generation
	// rather than the registration-time config.
	configs map[string]domain.StreamSourceConfig

	cancels  map[string]context.CancelFunc
	dones    map[string]chan struct{}
	runtimes map[string]*streamRuntime

	// portClaims maps a claimed port to the stream holding it. It exists so a
	// conflict can name the holder; the authority on whether a port is
	// available is the bind in source.Open, which also sees ports held by
	// processes outside Nephtys. A claim covers a stream's whole registered
	// life rather than one session, because it belongs to the configuration
	// and a restart has to be able to take the port back.
	portClaims map[string]string

	broker *broker.Broker
	store  configStore // nil in tests
	logger *slog.Logger

	// now and sleep are the supervisor's clock. Tests replace them so a
	// backoff ladder or a reset window can be exercised without waiting for
	// one, and without depending on the scheduler to be prompt.
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) bool
}

// NewStreamManager creates a manager backed by the given broker and store.
// The store may be nil (e.g. in unit tests), in which case persistence is disabled.
func NewStreamManager(brk *broker.Broker, st configStore) *StreamManager {
	return &StreamManager{
		generations: make(map[string]*atomic.Pointer[pipeline.Generation]),
		configs:     make(map[string]domain.StreamSourceConfig),
		cancels:     make(map[string]context.CancelFunc),
		dones:       make(map[string]chan struct{}),
		runtimes:    make(map[string]*streamRuntime),
		portClaims:  make(map[string]string),
		broker:      brk,
		store:       st,
		logger:      slog.With("component", "manager"),
		now:         time.Now,
		sleep:       sleepCtx,
	}
}

// StreamInfo is the API representation of a registered stream.
//
// RestartCount is cumulative over the stream's whole life, so it only ever
// grows. It is not the stream's position in its current restart budget: that
// resets whenever a session stays up long enough to earn its attempts back, and
// a field named restart_count that can decrease would be a poor thing to have
// to keep compatible.
type StreamInfo struct {
	ID            string              `json:"id"`
	Status        domain.SourceStatus `json:"status"`
	Health        string              `json:"health"`
	LastMessageAt *time.Time          `json:"last_message_at,omitempty"`
	RestartCount  int                 `json:"restart_count"`
	LastError     string              `json:"last_error,omitempty"`
	LastErrorAt   *time.Time          `json:"last_error_at,omitempty"`
}

// streamRuntime holds the per-stream facts the supervisor writes and the API
// reads. Every field is atomic on purpose: the supervisor must never take the
// manager lock, because Remove holds that lock while waiting for the supervisor
// to finish.
type streamRuntime struct {
	lastMessageUnixNano atomic.Int64
	status              atomic.Value // domain.SourceStatus
	restarts            atomic.Int64
	lastError           atomic.Pointer[streamFailure]
}

// errSessionClosed stands in when a session ends without an error of its own —
// a clean EOF from a source that was supposed to stay open. A stream that gives
// up has to be able to say why, so the failure contract cannot depend on every
// connector remembering to return something.
var errSessionClosed = errors.New("session ended without reporting an error")

type streamFailure struct {
	message string
	at      time.Time
}

func newStreamRuntime(status domain.SourceStatus) *streamRuntime {
	rt := &streamRuntime{}
	rt.status.Store(status)
	return rt
}

func (r *streamRuntime) getStatus() domain.SourceStatus {
	status, _ := r.status.Load().(domain.SourceStatus)
	return status
}

func (r *streamRuntime) setStatus(id string, status domain.SourceStatus) {
	r.status.Store(status)
	telemetry.SetStreamState(id, metricState(status))
}

// recordFailure stores the reason a session or an acquisition failed, in the
// form the API is allowed to serve. A nil error still records something: a
// stream that gives up has to be able to say why, and that cannot depend on
// every connector remembering to return an error when its session ends.
func (r *streamRuntime) recordFailure(err error, at time.Time) {
	if err == nil {
		err = errSessionClosed
	}
	r.lastError.Store(&streamFailure{message: redactForAPI(err.Error()), at: at})
}

// Errors the manager returns for conditions the REST layer maps to a specific
// status. Anything else is a failure to apply an otherwise valid request — a
// dependency problem rather than the caller's fault — and maps to 503.
var (
	ErrStreamNotFound = errors.New("stream not found")
	ErrStreamExists   = errors.New("stream already registered")

	// ErrPortConflict reports a port another registered stream already holds.
	ErrPortConflict = errors.New("port already claimed")

	// ErrSourceOpen reports a source that could not acquire its local
	// resources. The wrapped error carries the detail, including the address
	// when a bind is what failed.
	ErrSourceOpen = errors.New("stream resources unavailable")
)

// Register admits a source and starts it under a supervisor.
//
// It returns only once the stream's local resources are held: the port is
// bound, the pipeline is built, and the config is durable. It does not wait for
// an upstream connection, which is remote and may take arbitrarily long — so a
// successful return means the stream is ingesting or will report why it is not,
// rather than merely that a goroutine exists.
func (m *StreamManager) Register(source connector.StreamSource, cfg domain.StreamSourceConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.admitLocked(source, cfg, true)
}

// admitLocked performs admission and installs the stream. Nothing is left
// behind when it returns an error: no claim, no persisted config, no acquired
// listener. Must be called with mu held.
func (m *StreamManager) admitLocked(source connector.StreamSource, cfg domain.StreamSourceConfig, persist bool) error {
	id := source.ID()
	if _, exists := m.runtimes[id]; exists {
		return fmt.Errorf("source %q: %w", id, ErrStreamExists)
	}

	port := connectorPort(cfg)
	if port != "" {
		if holder, claimed := m.portClaims[port]; claimed {
			return fmt.Errorf("%w: port %s is held by stream %q", ErrPortConflict, port, holder)
		}
	}

	gen, err := pipeline.NewGeneration(id, cfg.Pipeline, m.publishFunc(id))
	if err != nil {
		return fmt.Errorf("build pipeline: %w", err)
	}

	// Acquire before persisting. The other order writes a config for a stream
	// that may not be startable and then has to delete it again, which is a
	// rollback that can itself fail.
	openCtx, cancelOpen := context.WithTimeout(context.Background(), admissionTimeout)
	defer cancelOpen()
	if err := source.Open(openCtx); err != nil {
		m.discardGeneration(id, gen)
		return fmt.Errorf("%w: %v", ErrSourceOpen, err)
	}

	if persist && m.store != nil {
		if err := m.store.Put(cfg); err != nil {
			source.Close()
			m.discardGeneration(id, gen)
			return fmt.Errorf("persist config: %w", err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	runtime := newStreamRuntime(domain.StatusConnecting)

	m.configs[id] = cfg
	m.cancels[id] = cancel
	m.dones[id] = done
	m.runtimes[id] = runtime
	if port != "" {
		m.portClaims[port] = id
	}

	var ref atomic.Pointer[pipeline.Generation]
	ref.Store(gen)
	m.generations[id] = &ref

	runtime.setStatus(id, domain.StatusConnecting)

	instrumentedPublish := func(topic string, event domain.StreamEvent) error {
		start := time.Now()
		runtime.lastMessageUnixNano.Store(start.UTC().UnixNano())
		telemetry.EventsIngested.WithLabelValues(id).Inc()
		telemetry.BytesIngested.WithLabelValues(id).Add(float64(eventPayloadSize(event)))

		err := ref.Load().Publish(topic, event)
		telemetry.EventProcessingDuration.WithLabelValues(id).Observe(time.Since(start).Seconds())
		return err
	}

	policy := restartPolicyFor(cfg)
	go func() {
		defer close(done)
		m.supervise(ctx, id, source, runtime, policy, connector.PublishFunc(instrumentedPublish))
	}()

	m.logger.Info("Stream admitted", "id", id, "kind", cfg.Kind, "restart_policy", policy)
	return nil
}

// admitFailedLocked registers a stream that could not be admitted, in a
// terminal failed state. Restore uses it: at startup there is no caller to
// answer, and a config that stays in the store while its stream is absent from
// the API is the least useful outcome available — the operator needs to see
// the stream and the reason it is down.
func (m *StreamManager) admitFailedLocked(cfg domain.StreamSourceConfig, cause error) {
	id := cfg.ID
	runtime := newStreamRuntime(domain.StatusError)
	runtime.recordFailure(cause, m.now())

	m.configs[id] = cfg
	m.runtimes[id] = runtime
	if port := connectorPort(cfg); port != "" {
		if _, claimed := m.portClaims[port]; !claimed {
			m.portClaims[port] = id
		}
	}
	runtime.setStatus(id, domain.StatusError)

	m.logger.Warn("Stream restored in failed state", "id", id, "error", cause)
}

// discardGeneration retires a generation that was built for a stream which was
// never installed, and drops the dedup series building it published.
func (m *StreamManager) discardGeneration(id string, gen *pipeline.Generation) {
	gen.Retire()
	telemetry.DeleteDedupSeries(id)
}

// supervise runs a stream's sessions and its restart policy. It is the only
// retry loop in the process: connectors run one session and return, so the
// backoff ladder and the attempt budget live here rather than in five
// connectors with five different opinions.
//
// It never takes m.mu. Remove holds that lock while waiting for this goroutine
// to finish, so reaching for it here would deadlock.
func (m *StreamManager) supervise(
	ctx context.Context,
	id string,
	source connector.StreamSource,
	runtime *streamRuntime,
	policy restartPolicy,
	publish connector.PublishFunc,
) {
	attempt := 0
	// Register already opened the source and answered the caller with the
	// result, so the first session starts without re-acquiring anything. Every
	// later pass through the loop is a restart, and has to acquire again.
	reacquire := false

	for {
		if reacquire {
			runtime.setStatus(id, domain.StatusReconnecting)
			if !m.sleep(ctx, policy.delay(attempt)) {
				runtime.setStatus(id, domain.StatusStopped)
				return
			}
			if err := source.Open(ctx); err != nil {
				// Acquisition can fail because the operator is stopping the
				// stream. Cancellation is not a failed attempt: without this
				// check a removal racing a restart would spend budget and
				// could leave the stream reported as errored on its way out.
				if ctx.Err() != nil {
					runtime.setStatus(id, domain.StatusStopped)
					return
				}
				m.logger.Warn("Restart could not acquire resources", "id", id, "attempt", attempt, "error", err)
				runtime.recordFailure(err, m.now())
				next, ok := policy.next(attempt)
				if !ok {
					m.markFailed(id, runtime)
					return
				}
				attempt = next
				runtime.restarts.Add(1)
				telemetry.StreamRestarts.WithLabelValues(id).Inc()
				continue
			}
		}
		reacquire = true

		runtime.setStatus(id, domain.StatusConnecting)

		var readyAt atomic.Int64
		runErr := source.Run(ctx, publish, func() {
			readyAt.Store(m.now().UnixNano())
			runtime.setStatus(id, domain.StatusRunning)
		})
		source.Close()

		// Cancellation is never a restart: an operator stopping the stream must
		// not race the supervisor into rebinding a port it is giving up.
		if ctx.Err() != nil {
			runtime.setStatus(id, domain.StatusStopped)
			return
		}

		if ra := readyAt.Load(); ra > 0 && m.now().Sub(time.Unix(0, ra)) >= policy.resetAfter {
			// The session earned its budget back by staying up, not by
			// connecting: a source that accepts and immediately drops would
			// otherwise never exhaust any budget.
			attempt = 0
		}
		// The log keeps the connector's own error, unredacted; the runtime
		// keeps a reason the API can serve, which is never empty.
		m.logger.Warn("Stream session ended", "id", id, "error", runErr)
		runtime.recordFailure(runErr, m.now())

		next, ok := policy.next(attempt)
		if !ok {
			m.markFailed(id, runtime)
			return
		}
		attempt = next
		runtime.restarts.Add(1)
		telemetry.StreamRestarts.WithLabelValues(id).Inc()
	}
}

// markFailed puts a stream in its terminal failed state. The stream stays
// registered and its config stays stored: it was a valid configuration, and
// dropping it would erase both the evidence and the operator's ability to see
// what happened.
func (m *StreamManager) markFailed(id string, runtime *streamRuntime) {
	runtime.setStatus(id, domain.StatusError)
	failure := runtime.lastError.Load()
	reason := ""
	if failure != nil {
		reason = failure.message
	}
	m.logger.Error("Stream failed permanently",
		"id", id, "restarts", runtime.restarts.Load(), "error", reason)
}

// sleepCtx waits for d, reporting false if ctx was cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// Remove stops and removes a source by ID, also deleting its persisted config.
func (m *StreamManager) Remove(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.runtimes[id]; !exists {
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

	if cancel, ok := m.cancels[id]; ok {
		cancel()
	}

	// Wait for the supervisor to finish shutting down.
	if done, ok := m.dones[id]; ok {
		<-done
	}

	// Retire only once the source has stopped publishing, so the pipeline's
	// final flush is the last thing that happens on this stream. Retire blocks
	// until every event the pipeline accepted has been handed to the broker. A
	// stream that failed admission at restore never built one.
	if ref, ok := m.generations[id]; ok {
		ref.Load().Retire()
	}

	m.releaseClaimsLocked(id)
	delete(m.generations, id)
	delete(m.configs, id)
	delete(m.cancels, id)
	delete(m.dones, id)
	delete(m.runtimes, id)
	telemetry.DeleteStreamSeries(id)

	m.logger.Info("Source removed", "id", id)
	return nil
}

// releaseClaimsLocked drops every port claim held by a stream. Must be called
// with mu held.
func (m *StreamManager) releaseClaimsLocked(id string) {
	for port, holder := range m.portClaims {
		if holder == id {
			delete(m.portClaims, port)
		}
	}
}

// Restore loads persisted stream configs from the store and re-registers them.
// Called once on startup to resume streams from a previous run.
//
// It runs before the REST API starts listening, which is what makes "restore
// complete" a meaningful moment: the API never serves a request while the
// stream set is half-built.
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

	// Admit in a stable order. Two persisted streams can claim the same port —
	// the store holds whatever was accepted over time — and without an order,
	// which one wins is map iteration luck that changes between restarts.
	sort.Slice(configs, func(i, j int) bool { return configs[i].ID < configs[j].ID })

	m.mu.Lock()
	defer m.mu.Unlock()

	restored, failed := 0, 0
	for _, cfg := range configs {
		if cfg.ID == "" {
			m.logger.Warn("Skipping persisted stream with no id")
			continue
		}

		// Re-validate on the way in. The KV bucket holds whatever the version
		// that wrote it accepted, so without this check restore is the one path
		// that can start a stream from configuration the current validator
		// rejects — the config contract would hold for new streams and quietly
		// not hold for surviving ones.
		if err := validateStreamConfig(cfg); err != nil {
			m.admitFailedLocked(cfg, fmt.Errorf("invalid persisted config: %w", err))
			failed++
			continue
		}
		source, err := sourceFromConfig(cfg)
		if err != nil {
			m.admitFailedLocked(cfg, err)
			failed++
			continue
		}
		if err := m.admitLocked(source, cfg, false); err != nil {
			m.admitFailedLocked(cfg, err)
			failed++
			continue
		}
		restored++
		m.logger.Info("Stream restored", "id", cfg.ID, "kind", cfg.Kind)
	}

	m.logger.Info("Restore complete", "restored", restored, "failed", failed, "persisted", len(configs))
	return nil
}

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
//
// The generation belongs to the stream, not to the session, so this works the
// same whether the stream is connected or waiting out a restart backoff, and a
// restart never rebuilds what it swaps.
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

// List returns info about all registered streams, including any that are
// registered in a terminal failed state.
func (m *StreamManager) List() []StreamInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()

	infos := make([]StreamInfo, 0, len(m.runtimes))
	for id, runtime := range m.runtimes {
		infos = append(infos, streamInfo(id, runtime))
	}
	return infos
}

// StreamDetail is StreamInfo plus the configuration the stream is actually
// running: the one it was registered with, amended by every accepted pipeline
// update. Until this existed the effective pipeline lived only inside a
// closure, so a hot-swap could be requested but never confirmed from outside
// the process.
//
// Config is redacted — see redactConfig. It describes a running stream; it is
// not a document that can be POSTed back.
type StreamDetail struct {
	StreamInfo
	Config domain.StreamSourceConfig `json:"config"`
}

// Describe returns one stream's info together with its effective, redacted
// config. The second return reports whether the stream is registered, which
// includes streams registered in a terminal failed state — those are exactly
// the ones an operator most needs to read the config of.
func (m *StreamManager) Describe(id string) (StreamDetail, bool) {
	m.mu.RLock()
	runtime, ok := m.runtimes[id]
	cfg := m.configs[id]
	m.mu.RUnlock()

	if !ok {
		return StreamDetail{}, false
	}

	// Both halves are read outside the lock. streamInfo reads atomics, and the
	// sub-configs cfg points at are never mutated in place — UpdatePipeline
	// installs a whole new PipelineConfig rather than editing the old one — so
	// what redactConfig walks cannot change underneath it.
	return StreamDetail{
		StreamInfo: streamInfo(id, runtime),
		Config:     redactConfig(cfg),
	}, true
}

// StatusOf reports a stream's current lifecycle status.
func (m *StreamManager) StatusOf(id string) (domain.SourceStatus, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	runtime, ok := m.runtimes[id]
	if !ok {
		return "", false
	}
	return runtime.getStatus(), true
}

func streamInfo(id string, runtime *streamRuntime) StreamInfo {
	status := runtime.getStatus()
	info := StreamInfo{
		ID:            id,
		Status:        status,
		Health:        sourceHealth(status),
		LastMessageAt: runtimeLastMessageAt(runtime),
		RestartCount:  int(runtime.restarts.Load()),
	}
	if failure := runtime.lastError.Load(); failure != nil {
		at := failure.at.UTC()
		info.LastError = failure.message
		info.LastErrorAt = &at
	}
	return info
}

// StopAll gracefully stops all registered sources.
func (m *StreamManager) StopAll() {
	m.mu.Lock()
	defer m.mu.Unlock()

	var wg sync.WaitGroup

	for id, cancel := range m.cancels {
		cancel()
		if done, ok := m.dones[id]; ok {
			wg.Add(1)
			go func(d chan struct{}) {
				defer wg.Done()
				<-d
			}(done)
		}
		m.logger.Info("Source stopped", "id", id)
	}

	// Wait for all supervisors to cleanly exit
	wg.Wait()

	// Retire each pipeline only after its source has stopped, and wait for the
	// final flush: on shutdown this is what stands between a buffered batch and
	// the process exiting without publishing it. A stream that failed admission
	// at restore has a runtime but no generation, so the series are cleared per
	// runtime rather than per generation.
	for _, ref := range m.generations {
		ref.Load().Retire()
	}
	// Every per-stream series, not just the state gauge: after StopAll the
	// manager holds no streams, so leaving counters behind would report on
	// streams that no longer exist if the process outlives the shutdown.
	for id := range m.runtimes {
		telemetry.DeleteStreamSeries(id)
	}

	m.generations = make(map[string]*atomic.Pointer[pipeline.Generation])
	m.configs = make(map[string]domain.StreamSourceConfig)
	m.cancels = make(map[string]context.CancelFunc)
	m.dones = make(map[string]chan struct{})
	m.runtimes = make(map[string]*streamRuntime)
	m.portClaims = make(map[string]string)
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

// connectorPort reports the local port a config claims, or "" for a kind that
// binds nothing.
func connectorPort(cfg domain.StreamSourceConfig) string {
	switch cfg.Kind {
	case "webhook":
		if cfg.Webhook != nil && cfg.Webhook.Port != "" {
			return canonicalPort(cfg.Webhook.Port)
		}
		return "8081" // the default NewWebhookSource applies
	case "grpc":
		if cfg.Grpc != nil && cfg.Grpc.Port != "" {
			return canonicalPort(cfg.Grpc.Port)
		}
		return "50051" // the default NewGrpcSource applies
	default:
		return ""
	}
}

// canonicalPort normalises a port so two spellings of one port cannot hold two
// different claims. The validator accepts "8081" and "08081" alike — Atoi does
// — and both bind the same socket, so keying claims on the raw string would let
// the second stream past the registry and into a bind failure that cannot name
// the holder.
func canonicalPort(raw string) string {
	port, err := strconv.Atoi(raw)
	if err != nil {
		// Not a number: the validator rejects this before a claim is made, and
		// binding it will fail too. Key it on what was written.
		return raw
	}
	return strconv.Itoa(port)
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
