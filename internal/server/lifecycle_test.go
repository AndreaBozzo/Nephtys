package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"nephtys/internal/connector"
	"nephtys/internal/domain"
	"nephtys/internal/telemetry"
)

// --- helpers -----------------------------------------------------------

// memStore is an in-memory configStore. The port-conflict tests have to state
// what was persisted, not merely that registration failed.
type memStore struct {
	mu      sync.Mutex
	order   []string
	configs map[string]domain.StreamSourceConfig
}

func newMemStore() *memStore {
	return &memStore{configs: make(map[string]domain.StreamSourceConfig)}
}

func (s *memStore) Put(cfg domain.StreamSourceConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.configs[cfg.ID]; !exists {
		s.order = append(s.order, cfg.ID)
	}
	s.configs[cfg.ID] = cfg
	return nil
}

func (s *memStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.configs, id)
	for i, existing := range s.order {
		if existing == id {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	return nil
}

func (s *memStore) List() ([]domain.StreamSourceConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Insertion order, deliberately not sorted: a test that wants Restore to
	// impose an order has to see it imposed rather than inherited.
	out := make([]domain.StreamSourceConfig, 0, len(s.configs))
	for _, id := range s.order {
		out = append(out, s.configs[id])
	}
	return out, nil
}

func (s *memStore) has(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.configs[id]
	return ok
}

// scriptedSource is a StreamSource whose acquisitions and sessions are driven
// by the test. Once its script is exhausted a session parks on the context
// instead of returning, so a supervisor that restarts more often than the test
// expects parks rather than spinning — the assertion fails on the count, not on
// a hot loop.
type scriptedSource struct {
	id string

	mu          sync.Mutex
	openErrs    []error
	sessionErrs []error
	opens       int
	sessions    int
	closes      int
}

var errSessionEnded = errors.New("simulated session failure")

func newScriptedSource(id string, openErrs, sessionErrs []error) *scriptedSource {
	return &scriptedSource{id: id, openErrs: openErrs, sessionErrs: sessionErrs}
}

func (s *scriptedSource) ID() string { return s.id }

func (s *scriptedSource) Open(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opens++
	if len(s.openErrs) > 0 {
		err := s.openErrs[0]
		s.openErrs = s.openErrs[1:]
		return err
	}
	return nil
}

func (s *scriptedSource) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closes++
}

func (s *scriptedSource) Run(ctx context.Context, _ connector.PublishFunc, ready connector.ReadyFunc) error {
	s.mu.Lock()
	s.sessions++
	scripted := len(s.sessionErrs) > 0
	var err error
	if scripted {
		err = s.sessionErrs[0]
		s.sessionErrs = s.sessionErrs[1:]
	}
	s.mu.Unlock()

	ready()

	if !scripted {
		<-ctx.Done()
		return nil
	}
	return err
}

func (s *scriptedSource) counts() (opens, sessions, closes int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.opens, s.sessions, s.closes
}

// testManager builds a manager whose supervisor does not wait: backoff is
// skipped and the clock is the test's. A backoff ladder asserted against the
// wall clock is a flake with a due date.
func testManager(t *testing.T, st configStore, clock func() time.Time) *StreamManager {
	t.Helper()
	m := NewStreamManager(nil, st)
	m.now = clock
	m.sleep = func(ctx context.Context, _ time.Duration) bool { return ctx.Err() == nil }
	t.Cleanup(m.StopAll)
	return m
}

// frozenClock never advances, so no session ever earns its restart budget back.
func frozenClock() func() time.Time {
	at := time.Date(2026, time.August, 26, 12, 0, 0, 0, time.UTC)
	return func() time.Time { return at }
}

// steppingClock advances by step on every reading, so any session that reports
// ready has, by the time it ends, been up for longer than any reset window the
// test configures.
func steppingClock(step time.Duration) func() time.Time {
	var calls atomic.Int64
	base := time.Date(2026, time.August, 26, 12, 0, 0, 0, time.UTC)
	return func() time.Time { return base.Add(time.Duration(calls.Add(1)) * step) }
}

func intPtr(v int) *int { return &v }

func infoFor(t *testing.T, m *StreamManager, id string) StreamInfo {
	t.Helper()
	for _, info := range m.List() {
		if info.ID == id {
			return info
		}
	}
	t.Fatalf("stream %q is not listed", id)
	return StreamInfo{}
}

func waitForSessions(t *testing.T, src *scriptedSource, want int) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		if _, sessions, _ := src.counts(); sessions >= want {
			return
		}
		select {
		case <-deadline:
			_, sessions, _ := src.counts()
			t.Fatalf("source ran %d sessions, want %d", sessions, want)
		case <-time.After(time.Millisecond):
		}
	}
}

// heldPort binds the wildcard address and keeps it, returning the port a
// connector would have to take. The wildcard matters: holding 127.0.0.1:P does
// not conflict with 0.0.0.0:P on every platform, so a loopback holder would let
// the test pass for the wrong reason.
func heldPort(t *testing.T) (string, net.Listener) {
	t.Helper()
	holder, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("bind holder: %v", err)
	}
	_, port, err := net.SplitHostPort(holder.Addr().String())
	if err != nil {
		t.Fatalf("split holder addr: %v", err)
	}
	t.Cleanup(func() { _ = holder.Close() })
	return port, holder
}

func webhookCfg(id, port string) domain.StreamSourceConfig {
	return domain.StreamSourceConfig{
		ID:      id,
		Kind:    "webhook",
		Topic:   "nephtys.stream.hooks",
		Webhook: &domain.WebhookConfig{Port: port, Path: "/hook"},
	}
}

// --- #59: registration means the connector started ---------------------

// TestRegister_PortHeldByAnotherStream is the first acceptance criterion of
// #59: two streams claiming one port is a configuration the manager used to
// accept, answering 201 for the second while its bind failed in a goroutine.
func TestRegister_PortHeldByAnotherStream(t *testing.T) {
	st := newMemStore()
	m := testManager(t, st, frozenClock())

	port := freePort(t)
	first := webhookCfg("first-holder", port)
	if err := m.Register(connector.NewWebhookSource(first.ID, first.Topic, first.Webhook), first); err != nil {
		t.Fatalf("register first: %v", err)
	}

	second := webhookCfg("second-claimant", port)
	err := m.Register(connector.NewWebhookSource(second.ID, second.Topic, second.Webhook), second)
	if err == nil {
		t.Fatal("registering a second stream on a claimed port succeeded")
	}
	if !errors.Is(err, ErrPortConflict) {
		t.Errorf("error %v is not an ErrPortConflict", err)
	}
	if !strings.Contains(err.Error(), port) {
		t.Errorf("error %q does not name the port %s", err, port)
	}
	if !strings.Contains(err.Error(), first.ID) {
		t.Errorf("error %q does not name the holding stream %q", err, first.ID)
	}

	if st.has(second.ID) {
		t.Error("a config was persisted for a stream that was never admitted")
	}
	if len(m.List()) != 1 {
		t.Errorf("listed %d streams, want only the one that holds the port", len(m.List()))
	}
}

// TestRegister_PortHeldOutsideNephtys is the second acceptance criterion of
// #59. The claim registry cannot see this one — the port belongs to another
// process — so it is the bind in Open that has to fail the request.
func TestRegister_PortHeldOutsideNephtys(t *testing.T) {
	st := newMemStore()
	m := testManager(t, st, frozenClock())

	port, _ := heldPort(t)
	cfg := webhookCfg("outsider", port)

	err := m.Register(connector.NewWebhookSource(cfg.ID, cfg.Topic, cfg.Webhook), cfg)
	if err == nil {
		t.Fatal("registering on a port held by another process succeeded")
	}
	if !errors.Is(err, ErrSourceOpen) {
		t.Errorf("error %v is not an ErrSourceOpen", err)
	}
	if !strings.Contains(err.Error(), port) {
		t.Errorf("error %q does not name the port %s", err, port)
	}
	if st.has(cfg.ID) {
		t.Error("a config was persisted for a stream that could not bind")
	}
	if len(m.List()) != 0 {
		t.Errorf("listed %d streams after a failed registration, want 0", len(m.List()))
	}
}

// TestCreateStream_PortConflictIsNot201 is the same two criteria at the REST
// surface, which is where #59 is actually observed: the request has to fail
// rather than answer 201 Created.
func TestCreateStream_PortConflictIsNot201(t *testing.T) {
	m := testManager(t, newMemStore(), frozenClock())
	s := &Server{manager: m}

	port, _ := heldPort(t)
	body, err := json.Marshal(webhookCfg("rest-outsider", port))
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}

	rec := httptest.NewRecorder()
	s.handleCreateStream(rec, httptest.NewRequest(http.MethodPost, "/v1/streams", bytes.NewReader(body)))

	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want %d (body: %s)", rec.Code, http.StatusConflict, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "started") {
		t.Errorf("body %q reports the stream as started", rec.Body.String())
	}
	if len(m.List()) != 0 {
		t.Error("a stream was registered despite the failed request")
	}
}

// TestCreateStream_ReportsLifecycleState checks the other half of the
// registration contract at the REST surface: 201 now carries the state the
// stream is actually in, so a caller can tell a connected stream from one
// still dialling instead of inferring either from the status code.
func TestCreateStream_ReportsLifecycleState(t *testing.T) {
	m := testManager(t, newMemStore(), frozenClock())
	s := &Server{manager: m}

	cfg := webhookCfg("state-in-body", freePort(t))
	body, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}

	rec := httptest.NewRecorder()
	s.handleCreateStream(rec, httptest.NewRequest(http.MethodPost, "/v1/streams", bytes.NewReader(body)))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusCreated, rec.Body.String())
	}

	var decoded map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	// The legacy fields are unchanged; state is additive.
	if decoded["id"] != cfg.ID || decoded["status"] != "started" {
		t.Errorf("body = %v, want the existing id and status fields intact", decoded)
	}
	state := domain.SourceStatus(decoded["state"])
	if state != domain.StatusConnecting && state != domain.StatusRunning {
		t.Errorf("state = %q, want connecting or running", state)
	}
}

// TestNoStreamRunsWhileItsListenerIsUnbound is #59's third acceptance
// criterion. It asserts the claim rather than a proxy for it: a stream that
// reports running is one whose endpoint actually answers, and a stream whose
// port was taken reports nothing at all because it was never admitted.
func TestNoStreamRunsWhileItsListenerIsUnbound(t *testing.T) {
	m := testManager(t, newMemStore(), frozenClock())

	// A stream that does come up: running has to mean bound and serving.
	live := webhookCfg("live-hook", freePort(t))
	if err := m.Register(connector.NewWebhookSource(live.ID, live.Topic, live.Webhook), live); err != nil {
		t.Fatalf("register live: %v", err)
	}
	waitForStatus(t, m, live.ID, domain.StatusRunning)

	// A GET reaches the handler and is refused by it, which proves the server
	// is bound and serving without needing a broker behind the publish path.
	res, err := http.Get("http://127.0.0.1:" + live.Webhook.Port + live.Webhook.Path)
	if err != nil {
		t.Fatalf("stream reports running but its endpoint refused the request: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("endpoint of a running stream answered %d, want %d", res.StatusCode, http.StatusMethodNotAllowed)
	}

	// A stream whose port is held elsewhere: bind first, register second.
	port, _ := heldPort(t)
	blocked := webhookCfg("blocked-hook", port)
	if err := m.Register(connector.NewWebhookSource(blocked.ID, blocked.Topic, blocked.Webhook), blocked); err == nil {
		t.Fatal("registration on a held port succeeded")
	}

	for _, info := range m.List() {
		if info.ID == blocked.ID {
			t.Fatalf("stream %q is listed as %q after a failed registration", info.ID, info.Status)
		}
		if info.Status == domain.StatusRunning && info.ID != live.ID {
			t.Errorf("stream %q reports running unexpectedly", info.ID)
		}
	}
}

// TestRegister_RollsBackOnPersistFailure checks the ordering admission relies
// on: resources are acquired before the config is written, so a store that
// refuses the write leaves nothing holding the port.
func TestRegister_RollsBackOnPersistFailure(t *testing.T) {
	st := &failingStore{inner: newMemStore(), failPut: true}
	m := testManager(t, st, frozenClock())

	port := freePort(t)
	cfg := webhookCfg("rollback", port)
	if err := m.Register(connector.NewWebhookSource(cfg.ID, cfg.Topic, cfg.Webhook), cfg); err == nil {
		t.Fatal("register succeeded despite the store refusing the write")
	}
	if len(m.List()) != 0 {
		t.Error("a stream was installed despite the failed write")
	}

	// The listener has to have been released: the same port must be bindable.
	st.failPut = false
	retry := webhookCfg("rollback-retry", port)
	if err := m.Register(connector.NewWebhookSource(retry.ID, retry.Topic, retry.Webhook), retry); err != nil {
		t.Fatalf("port was not released by the rolled-back registration: %v", err)
	}
}

// --- #15: connector supervisor -----------------------------------------

// TestSupervisor_GivesUpAfterMaxAttempts covers the give-up path: the budget is
// spent, the stream goes terminal, and it stays visible with the reason.
func TestSupervisor_GivesUpAfterMaxAttempts(t *testing.T) {
	st := newMemStore()
	m := testManager(t, st, frozenClock())

	src := newScriptedSource("give-up", nil, []error{errSessionEnded, errSessionEnded, errSessionEnded})
	cfg := mockConfig(src.id)
	cfg.Restart = &domain.RestartConfig{MaxAttempts: intPtr(2)}
	t.Cleanup(func() { telemetry.DeleteStreamSeries(src.id) })

	if err := m.Register(src, cfg); err != nil {
		t.Fatalf("register: %v", err)
	}
	waitForStatus(t, m, src.id, domain.StatusError)

	info := infoFor(t, m, src.id)
	if info.RestartCount != 2 {
		t.Errorf("restart_count = %d, want 2", info.RestartCount)
	}
	if info.Health != "errored" {
		t.Errorf("health = %q, want errored", info.Health)
	}
	if !strings.Contains(info.LastError, errSessionEnded.Error()) {
		t.Errorf("last_error = %q, want the session failure", info.LastError)
	}
	if info.LastErrorAt == nil {
		t.Error("a failed stream reports no last_error_at")
	}

	if got := testutil.ToFloat64(telemetry.StreamState.WithLabelValues(src.id, "errored")); got != 1 {
		t.Errorf("errored gauge = %v, want 1", got)
	}
	if got := testutil.ToFloat64(telemetry.StreamRestarts.WithLabelValues(src.id)); got != 2 {
		t.Errorf("restarts counter = %v, want 2", got)
	}

	opens, sessions, closes := src.counts()
	if sessions != 3 {
		t.Errorf("ran %d sessions, want 3 (one plus two restarts)", sessions)
	}
	if opens != 3 {
		t.Errorf("acquired %d times, want 3 — a restart has to re-acquire", opens)
	}
	if closes != 3 {
		t.Errorf("released %d times, want 3 — every session has to be closed", closes)
	}

	// A failed stream stays registered: dropping it would erase the evidence
	// and leave the runtime disagreeing with the store.
	if !st.has(src.id) {
		t.Error("the config of a failed stream was deleted")
	}
}

// TestSupervisor_RecoversWithinBudget covers the recovery path: a session that
// ends inside the budget is restarted and the stream comes back.
func TestSupervisor_RecoversWithinBudget(t *testing.T) {
	m := testManager(t, nil, frozenClock())

	src := newScriptedSource("recovers", nil, []error{errSessionEnded})
	cfg := mockConfig(src.id)
	cfg.Restart = &domain.RestartConfig{MaxAttempts: intPtr(3)}
	t.Cleanup(func() { telemetry.DeleteStreamSeries(src.id) })

	if err := m.Register(src, cfg); err != nil {
		t.Fatalf("register: %v", err)
	}
	waitForSessions(t, src, 2)
	waitForStatus(t, m, src.id, domain.StatusRunning)

	info := infoFor(t, m, src.id)
	if info.RestartCount != 1 {
		t.Errorf("restart_count = %d, want 1", info.RestartCount)
	}
	if info.Health != "healthy" {
		t.Errorf("health = %q, want healthy after recovery", info.Health)
	}
	// The failure that caused the restart stays readable: an operator looking
	// at a recovered stream still wants to know what happened to it.
	if info.LastError == "" {
		t.Error("a stream that restarted reports no last_error")
	}
}

// TestSupervisor_OpenFailureDuringRestartSpendsBudget covers the other half of
// a restart: re-acquiring can fail too, and that has to consume an attempt
// rather than loop for free.
func TestSupervisor_OpenFailureDuringRestartSpendsBudget(t *testing.T) {
	m := testManager(t, nil, frozenClock())

	src := newScriptedSource("reopen-fails",
		[]error{nil, errors.New("port taken by someone else now")},
		[]error{errSessionEnded})
	cfg := mockConfig(src.id)
	cfg.Restart = &domain.RestartConfig{MaxAttempts: intPtr(1)}
	t.Cleanup(func() { telemetry.DeleteStreamSeries(src.id) })

	if err := m.Register(src, cfg); err != nil {
		t.Fatalf("register: %v", err)
	}
	waitForStatus(t, m, src.id, domain.StatusError)

	info := infoFor(t, m, src.id)
	if !strings.Contains(info.LastError, "port taken") {
		t.Errorf("last_error = %q, want the acquisition failure", info.LastError)
	}
	if _, sessions, _ := src.counts(); sessions != 1 {
		t.Errorf("ran %d sessions, want 1 — the restart never got past Open", sessions)
	}
}

// TestSupervisor_FlapWithinResetWindowExhaustsBudget is the test that
// discriminates the reset rule. The connectors used to reset their attempt
// counter the moment a dial succeeded; under a bounded budget that lets a
// source which accepts and immediately drops restart forever, never reaching a
// state anyone can alert on. Reverting to a reset-on-connect rule makes this
// test hang rather than fail — which is the point.
func TestSupervisor_FlapWithinResetWindowExhaustsBudget(t *testing.T) {
	m := testManager(t, nil, frozenClock())

	src := newScriptedSource("flapper", nil, []error{errSessionEnded, errSessionEnded})
	cfg := mockConfig(src.id)
	cfg.Restart = &domain.RestartConfig{
		MaxAttempts: intPtr(1),
		ResetAfter:  "1m",
	}
	t.Cleanup(func() { telemetry.DeleteStreamSeries(src.id) })

	if err := m.Register(src, cfg); err != nil {
		t.Fatalf("register: %v", err)
	}
	waitForStatus(t, m, src.id, domain.StatusError)

	if info := infoFor(t, m, src.id); info.RestartCount != 1 {
		t.Errorf("restart_count = %d, want 1", info.RestartCount)
	}
	if _, sessions, _ := src.counts(); sessions != 2 {
		t.Errorf("ran %d sessions, want 2", sessions)
	}
}

// TestSupervisor_UptimeEarnsBudgetBack is the other side of the same rule: a
// stream that stays up for the reset window gets its attempts back, so an
// hourly reconnect never exhausts a small budget over a long run.
func TestSupervisor_UptimeEarnsBudgetBack(t *testing.T) {
	m := testManager(t, nil, steppingClock(time.Hour))

	src := newScriptedSource("long-lived", nil,
		[]error{errSessionEnded, errSessionEnded, errSessionEnded})
	cfg := mockConfig(src.id)
	cfg.Restart = &domain.RestartConfig{
		MaxAttempts: intPtr(1),
		ResetAfter:  "1m",
	}
	t.Cleanup(func() { telemetry.DeleteStreamSeries(src.id) })

	if err := m.Register(src, cfg); err != nil {
		t.Fatalf("register: %v", err)
	}

	// Three failures against a budget of one: only the reset rule keeps this
	// stream alive to run a fourth session.
	waitForSessions(t, src, 4)
	waitForStatus(t, m, src.id, domain.StatusRunning)

	// Three restarts happened, and restart_count says three. It counts the
	// stream's whole life rather than its position in the current budget —
	// that position went 1, 0, 1, 0, 1 as each session earned its attempts
	// back, and a field named restart_count that goes down would be a poor
	// thing to have promised.
	if info := infoFor(t, m, src.id); info.RestartCount != 3 {
		t.Errorf("restart_count = %d, want 3", info.RestartCount)
	}
}

// TestSupervisor_CancellationDuringBackoffDoesNotRestart uses the real sleeper
// with an hour-long ladder: removing a stream mid-backoff has to return
// promptly, without re-acquiring anything on the way out.
func TestSupervisor_CancellationDuringBackoffDoesNotRestart(t *testing.T) {
	m := NewStreamManager(nil, nil)
	m.now = frozenClock()

	src := newScriptedSource("cancel-in-backoff", nil, []error{errSessionEnded})
	cfg := mockConfig(src.id)
	cfg.Restart = &domain.RestartConfig{InitialBackoff: "1h", MaxBackoff: "1h"}
	t.Cleanup(func() { telemetry.DeleteStreamSeries(src.id) })

	if err := m.Register(src, cfg); err != nil {
		t.Fatalf("register: %v", err)
	}
	waitForStatus(t, m, src.id, domain.StatusReconnecting)

	removed := make(chan error, 1)
	go func() { removed <- m.Remove(src.id) }()

	select {
	case err := <-removed:
		if err != nil {
			t.Fatalf("remove: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Remove blocked on a supervisor waiting out its backoff")
	}

	if opens, sessions, _ := src.counts(); opens != 1 || sessions != 1 {
		t.Errorf("opens = %d, sessions = %d, want 1 and 1 — cancellation must not start a restart", opens, sessions)
	}
	if len(m.List()) != 0 {
		t.Error("stream still listed after removal")
	}
	assertStreamStateDeleted(t, src.id)
}

// TestSupervisor_PushDefaultIsBoundedThenTerminal pins the push default: a
// lost listener is retried a few times and then becomes terminal. Before there
// was a supervisor the first failure was terminal and an operator had to
// remove and re-register the stream by hand.
func TestSupervisor_PushDefaultIsBoundedThenTerminal(t *testing.T) {
	m := testManager(t, nil, frozenClock())

	failures := make([]error, defaultPushAttempts+1)
	for i := range failures {
		failures[i] = errSessionEnded
	}
	src := newScriptedSource("push-default", nil, failures)
	cfg := domain.StreamSourceConfig{ID: src.id, Kind: "webhook", Topic: "nephtys.stream.hooks"}
	t.Cleanup(func() { telemetry.DeleteStreamSeries(src.id) })

	if err := m.Register(src, cfg); err != nil {
		t.Fatalf("register: %v", err)
	}
	waitForStatus(t, m, src.id, domain.StatusError)

	if info := infoFor(t, m, src.id); info.RestartCount != defaultPushAttempts {
		t.Errorf("restart_count = %d, want %d", info.RestartCount, defaultPushAttempts)
	}
	opens, sessions, _ := src.counts()
	if sessions != defaultPushAttempts+1 {
		t.Errorf("ran %d sessions, want %d", sessions, defaultPushAttempts+1)
	}
	if opens != defaultPushAttempts+1 {
		t.Errorf("acquired %d times, want %d — every restart rebinds the listener", opens, defaultPushAttempts+1)
	}
}

// TestSupervisor_PushRecoversFromATransientListenerLoss is the case the
// non-zero default exists for: the listener comes back on the next attempt and
// nobody has to touch the stream.
func TestSupervisor_PushRecoversFromATransientListenerLoss(t *testing.T) {
	m := testManager(t, nil, frozenClock())

	src := newScriptedSource("push-recovers", nil, []error{errSessionEnded})
	cfg := domain.StreamSourceConfig{ID: src.id, Kind: "webhook", Topic: "nephtys.stream.hooks"}
	t.Cleanup(func() { telemetry.DeleteStreamSeries(src.id) })

	if err := m.Register(src, cfg); err != nil {
		t.Fatalf("register: %v", err)
	}
	waitForSessions(t, src, 2)
	waitForStatus(t, m, src.id, domain.StatusRunning)

	if info := infoFor(t, m, src.id); info.Health != "healthy" {
		t.Errorf("health = %q, want healthy after the listener came back", info.Health)
	}
}

// TestSupervisor_PullDefaultIsUnlimited pins the other default: the pull
// connectors used to retry forever inside themselves, and moving the loop into
// the supervisor must not turn that into a bounded budget by accident.
func TestSupervisor_PullDefaultIsUnlimited(t *testing.T) {
	m := testManager(t, nil, frozenClock())

	failures := make([]error, 20)
	for i := range failures {
		failures[i] = errSessionEnded
	}
	src := newScriptedSource("pull-default", nil, failures)
	t.Cleanup(func() { telemetry.DeleteStreamSeries(src.id) })

	if err := m.Register(src, mockConfig(src.id)); err != nil {
		t.Fatalf("register: %v", err)
	}
	waitForSessions(t, src, 21)
	waitForStatus(t, m, src.id, domain.StatusRunning)

	if info := infoFor(t, m, src.id); info.Status == domain.StatusError {
		t.Error("a websocket stream gave up, but its default policy is unlimited")
	}
}

// TestSupervisor_RestartKeepsPipelineGeneration checks that a restart replaces
// the session, not the stream. The generation is what buffers batched events,
// so rebuilding it on every reconnect would strand whatever it held.
func TestSupervisor_RestartKeepsPipelineGeneration(t *testing.T) {
	m := testManager(t, nil, frozenClock())

	src := newScriptedSource("keeps-generation", nil, []error{errSessionEnded})
	cfg := mockConfig(src.id)
	cfg.Restart = &domain.RestartConfig{MaxAttempts: intPtr(2)}
	t.Cleanup(func() { telemetry.DeleteStreamSeries(src.id) })

	if err := m.Register(src, cfg); err != nil {
		t.Fatalf("register: %v", err)
	}

	m.mu.RLock()
	before := m.generations[src.id].Load()
	m.mu.RUnlock()

	waitForSessions(t, src, 2)
	waitForStatus(t, m, src.id, domain.StatusRunning)

	m.mu.RLock()
	after := m.generations[src.id].Load()
	m.mu.RUnlock()

	if before != after {
		t.Error("the restart rebuilt the pipeline generation instead of reusing it")
	}
}

// --- restore parity ----------------------------------------------------

// TestRestore_AdmitsInSortedOrder covers a state the store can hold and the
// API cannot create: two persisted streams claiming one port. Which one wins
// has to be the same across restarts, or an operator diagnoses a bug that
// moves.
func TestRestore_AdmitsInSortedOrder(t *testing.T) {
	port := freePort(t)
	st := newMemStore()
	for _, cfg := range []domain.StreamSourceConfig{
		webhookCfg("z-last", port),
		webhookCfg("a-first", port),
	} {
		if err := st.Put(cfg); err != nil {
			t.Fatalf("put: %v", err)
		}
	}

	for round := 0; round < 2; round++ {
		m := testManager(t, st, frozenClock())
		if err := m.Restore(); err != nil {
			t.Fatalf("round %d restore: %v", round, err)
		}
		waitForStatus(t, m, "a-first", domain.StatusRunning)

		loser := infoFor(t, m, "z-last")
		if loser.Status != domain.StatusError {
			t.Errorf("round %d: %q status = %q, want %q", round, loser.ID, loser.Status, domain.StatusError)
		}
		if !strings.Contains(loser.LastError, port) {
			t.Errorf("round %d: %q last_error = %q, want the port conflict", round, loser.ID, loser.LastError)
		}
		m.StopAll()
	}
}

// TestRestore_UnbindableStreamStaysVisible is the restore half of the failure
// contract. Registration can answer its caller; restore cannot, so the failure
// has to become a state rather than a log line and a missing row.
func TestRestore_UnbindableStreamStaysVisible(t *testing.T) {
	port, _ := heldPort(t)
	st := newMemStore()
	if err := st.Put(webhookCfg("blocked-on-boot", port)); err != nil {
		t.Fatalf("put: %v", err)
	}

	m := testManager(t, st, frozenClock())
	if err := m.Restore(); err != nil {
		t.Fatalf("restore: %v", err)
	}

	info := infoFor(t, m, "blocked-on-boot")
	if info.Status != domain.StatusError {
		t.Errorf("status = %q, want %q", info.Status, domain.StatusError)
	}
	if !strings.Contains(info.LastError, port) {
		t.Errorf("last_error = %q, want the bind failure naming port %s", info.LastError, port)
	}
	if !st.has("blocked-on-boot") {
		t.Error("restore deleted the config of a stream it could not start")
	}

	// A stream registered in a failed state is still an ordinary stream: the
	// operator has to be able to remove it.
	if err := m.Remove("blocked-on-boot"); err != nil {
		t.Fatalf("remove a failed stream: %v", err)
	}
	if st.has("blocked-on-boot") {
		t.Error("removing a failed stream left its config behind")
	}
}

// TestRestore_ReleasedPortIsReclaimable checks that a stream failing at restore
// does not permanently poison its port for a stream that can use it.
func TestRestore_ReleasedPortIsReclaimable(t *testing.T) {
	port, holder := heldPort(t)
	st := newMemStore()
	if err := st.Put(webhookCfg("boot-blocked", port)); err != nil {
		t.Fatalf("put: %v", err)
	}

	m := testManager(t, st, frozenClock())
	if err := m.Restore(); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if err := m.Remove("boot-blocked"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := holder.Close(); err != nil {
		t.Fatalf("close holder: %v", err)
	}

	cfg := webhookCfg("boot-blocked", port)
	if err := m.Register(connector.NewWebhookSource(cfg.ID, cfg.Topic, cfg.Webhook), cfg); err != nil {
		t.Fatalf("re-register on the freed port: %v", err)
	}
}
