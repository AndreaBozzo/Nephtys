package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"nephtys/internal/broker"
)

// stubBroker stands in for the NATS connection so a probe can be tested against
// a dependency that is down without taking a real broker down.
type stubBroker struct {
	connected bool
	state     string
	jetStream bool

	// jetStreamCalls counts the round trips the probe would have made, so a
	// test can assert the one it must not make.
	jetStreamCalls *int
}

func (b stubBroker) IsConnected() bool { return b.connected }
func (b stubBroker) ConnState() string { return b.state }
func (b stubBroker) JetStreamAvailable() bool {
	if b.jetStreamCalls != nil {
		*b.jetStreamCalls++
	}
	return b.jetStream
}

// connectedBroker is the everything-is-fine dependency.
func connectedBroker() stubBroker {
	return stubBroker{connected: true, state: "CONNECTED", jetStream: true}
}

// The real broker must keep satisfying the interface the probes are written
// against; the stub is only allowed to stand in for it while it does.
var _ brokerHealth = (*broker.Broker)(nil)

func probeServer(dep brokerHealth) *Server {
	return &Server{manager: NewStreamManager(nil, nil), broker: dep}
}

func decodeBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal %q: %v", w.Body.String(), err)
	}
	return body
}

func get(t *testing.T, h http.HandlerFunc, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w
}

// The point of the split: a broker outage must not make the process look dead.
func TestLivez_IgnoresBrokerOutage(t *testing.T) {
	for _, dep := range []stubBroker{
		connectedBroker(),
		{connected: false, state: "RECONNECTING"},
	} {
		s := probeServer(dep)
		w := get(t, s.handleLivez, "/livez")

		if w.Code != http.StatusOK {
			t.Errorf("broker connected=%v: expected 200, got %d", dep.connected, w.Code)
		}
		if got := decodeBody(t, w)["status"]; got != statusAlive {
			t.Errorf("broker connected=%v: expected status %q, got %v", dep.connected, statusAlive, got)
		}
	}
}

func TestReadyz_Connected(t *testing.T) {
	s := probeServer(connectedBroker())

	w := get(t, s.handleReadyz, "/readyz")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)
	if body["status"] != statusReady {
		t.Errorf("expected status %q, got %v", statusReady, body["status"])
	}
	if _, ok := body["reason"]; ok {
		t.Errorf("a ready instance should carry no reason, got %v", body["reason"])
	}
	checks, ok := body["checks"].(map[string]any)
	if !ok {
		t.Fatalf("expected checks object, got %v", body["checks"])
	}
	brokerCheck, ok := checks["broker"].(map[string]any)
	if !ok {
		t.Fatalf("expected broker check object, got %v", checks["broker"])
	}
	if brokerCheck["status"] != checkOK || brokerCheck["state"] != "CONNECTED" {
		t.Errorf("unexpected broker check: %v", brokerCheck)
	}
	jetStreamCheck, ok := checks["jetstream"].(map[string]any)
	if !ok {
		t.Fatalf("expected jetstream check object, got %v", checks["jetstream"])
	}
	if jetStreamCheck["status"] != checkOK {
		t.Errorf("unexpected jetstream check: %v", jetStreamCheck)
	}
}

// A connected broker whose JetStream is gone — disabled, unprovisioned for the
// account, or short of quorum — cannot take a registration or publish an event,
// so the connection being up is not enough to answer ready.
func TestReadyz_JetStreamUnavailable(t *testing.T) {
	s := probeServer(stubBroker{connected: true, state: "CONNECTED", jetStream: false})

	w := get(t, s.handleReadyz, "/readyz")

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d (%s)", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)
	if body["reason"] != reasonJetStreamUnavailable {
		t.Errorf("expected the jetstream reason, got %v", body["reason"])
	}
	checks := body["checks"].(map[string]any)
	if got := checks["broker"].(map[string]any)["status"]; got != checkOK {
		t.Errorf("the connection is up; broker check should still say %q, got %v", checkOK, got)
	}
	if got := checks["jetstream"].(map[string]any)["status"]; got != checkUnavailable {
		t.Errorf("expected jetstream %q, got %v", checkUnavailable, got)
	}
}

// With the connection down the JetStream round trip could only time out, so it
// is skipped and reported as unknown rather than as a second failure.
func TestReadyz_BrokerDownSkipsJetStreamProbe(t *testing.T) {
	calls := 0
	s := probeServer(stubBroker{connected: false, state: "RECONNECTING", jetStream: true, jetStreamCalls: &calls})

	w := get(t, s.handleReadyz, "/readyz")

	body := decodeBody(t, w)
	if body["reason"] != reasonBrokerUnavailable {
		t.Errorf("expected the broker reason to win, got %v", body["reason"])
	}
	checks := body["checks"].(map[string]any)
	if got := checks["jetstream"].(map[string]any)["status"]; got != checkUnknown {
		t.Errorf("expected jetstream %q when it was never asked, got %v", checkUnknown, got)
	}
	if calls != 0 {
		t.Errorf("probed JetStream %d times over a dead connection, want 0", calls)
	}
}

func TestReadyz_BrokerDown(t *testing.T) {
	s := probeServer(stubBroker{connected: false, state: "RECONNECTING"})

	w := get(t, s.handleReadyz, "/readyz")

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d (%s)", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)
	if body["status"] != statusUnready {
		t.Errorf("expected status %q, got %v", statusUnready, body["status"])
	}
	if body["reason"] != reasonBrokerUnavailable {
		t.Errorf("expected reason %q, got %v", reasonBrokerUnavailable, body["reason"])
	}
	checks := body["checks"].(map[string]any)
	brokerCheck := checks["broker"].(map[string]any)
	if brokerCheck["status"] != checkUnavailable {
		t.Errorf("expected broker check %q, got %v", checkUnavailable, brokerCheck["status"])
	}
	// The reconnect state is the diagnostic an operator needs to tell "the
	// broker went away and we are chasing it" from "we gave up".
	if brokerCheck["state"] != "RECONNECTING" {
		t.Errorf("expected connection state to be reported, got %v", brokerCheck["state"])
	}
}

// A 503 carries no credential and no operator-supplied text: every string in it
// is one of the package's own literals.
func TestReadyz_ReasonIsAClosedSet(t *testing.T) {
	allowed := map[string]bool{
		statusUnready:              true,
		reasonBrokerUnavailable:    true,
		reasonJetStreamUnavailable: true,
		checkUnavailable:           true,
		checkUnknown:               true,
		"DISCONNECTED":             true,
	}

	s := probeServer(stubBroker{connected: false, state: "DISCONNECTED"})
	w := get(t, s.handleReadyz, "/readyz")

	var walk func(v any)
	walk = func(v any) {
		switch t2 := v.(type) {
		case string:
			if !allowed[t2] {
				t.Errorf("unexpected free-form string in probe response: %q", t2)
			}
		case map[string]any:
			for _, sub := range t2 {
				walk(sub)
			}
		}
	}
	walk(decodeBody(t, w))
}

// /health is unchanged: still 200, still "degraded" rather than a failure code.
func TestHealth_UnchangedByProbeSplit(t *testing.T) {
	cases := []struct {
		connected bool
		status    string
		broker    string
	}{
		{true, "ok", "connected"},
		{false, "degraded", "disconnected"},
	}

	for _, tc := range cases {
		s := probeServer(stubBroker{connected: tc.connected, state: "CONNECTED", jetStream: tc.connected})
		w := get(t, s.handleHealth, "/health")

		if w.Code != http.StatusOK {
			t.Errorf("connected=%v: expected 200, got %d", tc.connected, w.Code)
		}
		body := decodeBody(t, w)
		if body["status"] != tc.status || body["broker"] != tc.broker {
			t.Errorf("connected=%v: expected {%q,%q}, got %v", tc.connected, tc.status, tc.broker, body)
		}
	}
}

// An orchestrator's probe carries no bearer token, so the probes must answer
// through the full middleware stack with auth configured.
func TestProbes_ExemptFromAdminAuth(t *testing.T) {
	srv := New("0", NewStreamManager(nil, nil), nil, "s3cret")
	srv.broker = connectedBroker()

	cases := []struct {
		path string
		code int
	}{
		{"/livez", http.StatusOK},
		{"/readyz", http.StatusOK},
		{"/health", http.StatusOK},
		{"/v1/streams", http.StatusUnauthorized},
	}

	for _, tc := range cases {
		w := httptest.NewRecorder()
		srv.httpServer.Handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if w.Code != tc.code {
			t.Errorf("%s: expected %d, got %d (%s)", tc.path, tc.code, w.Code, w.Body.String())
		}
	}
}
