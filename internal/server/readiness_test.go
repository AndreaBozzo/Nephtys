package server

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"

	"nephtys/internal/broker"
)

// startNATSOnPort starts an embedded NATS server on a chosen port, so an outage
// can be simulated by stopping the broker and bringing it back where the client
// is still looking for it.
func startNATSOnPort(t *testing.T, port int, storeDir string) *natsserver.Server {
	t.Helper()
	srv, err := natsserver.NewServer(&natsserver.Options{
		Host:      "127.0.0.1",
		Port:      port,
		JetStream: true,
		StoreDir:  storeDir,
	})
	if err != nil {
		t.Fatalf("nats server on port %d: %v", port, err)
	}
	srv.Start()
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatalf("nats server on port %d not ready", port)
	}
	t.Cleanup(srv.Shutdown)
	return srv
}

// A NATS server with JetStream switched off is the cheap, deterministic stand-in
// for every way JetStream can be absent on a live connection — disabled, not
// provisioned for the account, or short of quorum. The connection is up and core
// NATS works; nothing Nephtys needs to persist or publish does.
func TestReadyz_ConnectedWithoutJetStream(t *testing.T) {
	nats, err := natsserver.NewServer(&natsserver.Options{
		Host:      "127.0.0.1",
		Port:      -1,
		JetStream: false,
	})
	if err != nil {
		t.Fatalf("nats server: %v", err)
	}
	nats.Start()
	if !nats.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats not ready")
	}
	t.Cleanup(nats.Shutdown)

	brk, err := broker.Connect(nats.ClientURL(), broker.DefaultConfig())
	if err != nil {
		t.Fatalf("broker connect: %v", err)
	}
	t.Cleanup(brk.Close)

	if !brk.IsConnected() {
		t.Fatal("expected the connection itself to be up")
	}

	s := &Server{manager: NewStreamManager(brk, nil), broker: brk}
	w := httptest.NewRecorder()
	s.handleReadyz(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 with JetStream absent, got %d (%s)", w.Code, w.Body.String())
	}
	if body := w.Body.String(); !strings.Contains(body, reasonJetStreamUnavailable) {
		t.Errorf("expected the jetstream reason, got %s", body)
	}

	// /livez is unmoved: the process is fine, its dependency is not.
	lw := httptest.NewRecorder()
	s.handleLivez(lw, httptest.NewRequest(http.MethodGet, "/livez", nil))
	if lw.Code != http.StatusOK {
		t.Errorf("expected /livez to stay 200, got %d", lw.Code)
	}
}

// readyzCode drives the handler and reports the status code it answered with.
func readyzCode(t *testing.T, s *Server) int {
	t.Helper()
	w := httptest.NewRecorder()
	s.handleReadyz(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	return w.Code
}

// waitForReadyz polls the probe until it answers want, and reports what it saw
// last if it never does.
func waitForReadyz(t *testing.T, s *Server, want int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	got := 0
	for time.Now().Before(deadline) {
		if got = readyzCode(t, s); got == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("readyz never answered %d within %s (last: %d)", want, within, got)
}

// The acceptance criterion for the probe split: a broker outage makes the
// instance unready without making it look dead, and recovery restores readiness
// in the same process.
func TestReadyz_BrokerOutageAndRecovery(t *testing.T) {
	storeDir := t.TempDir()
	nats1 := startNATSOnPort(t, -1, storeDir)
	port := nats1.Addr().(*net.TCPAddr).Port

	brk := connectBroker(t, nats1)
	s := &Server{manager: NewStreamManager(brk, nil), broker: brk}

	if code := readyzCode(t, s); code != http.StatusOK {
		t.Fatalf("expected a connected instance to be ready, got %d", code)
	}

	nats1.Shutdown()
	nats1.WaitForShutdown()

	waitForReadyz(t, s, http.StatusServiceUnavailable, 10*time.Second)

	// Liveness must not have moved: nothing is wrong with this process.
	w := httptest.NewRecorder()
	s.handleLivez(w, httptest.NewRequest(http.MethodGet, "/livez", nil))
	if w.Code != http.StatusOK {
		t.Errorf("expected the process to stay live through a broker outage, got %d", w.Code)
	}

	startNATSOnPort(t, port, storeDir)

	// No restart, no new broker: the same *Server answers 200 again once the
	// client's own reconnect succeeds.
	waitForReadyz(t, s, http.StatusOK, 30*time.Second)
}
