package connector

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"nephtys/internal/domain"
	pb "nephtys/internal/grpc/streamer"
)

// Local fault fixtures for the conformance suite.
//
// Every fixture is an in-process upstream with the same three levers on it:
// deliver one well-formed event, deliver one malformed frame, and go away
// mid-session. Nothing here reaches the network beyond loopback and nothing
// sleeps for a fixed period, so the whole suite is deterministic in CI.
//
// The levers are what make faults reusable across connectors. A disconnect is
// a closed WebSocket for one connector and a handler returning for another;
// the suite asserts the same thing about both because the fixture, not the
// test, knows the difference.

// malformedMarker appears inside every malformed frame the fixtures deliver,
// so a test can pick the event derived from it out of a stream that may still
// be carrying well-formed events queued before the switch. Every connector
// either wraps the frame or passes it through, and both keep the marker.
const malformedMarker = "nephtys-malformed"

// malformedFrame is not valid JSON: the object is never closed.
const malformedFrame = `{"` + malformedMarker + `":`

// wellFormedFrame is the ordinary case the malformed one is read against.
const wellFormedFrame = `{"value":1}`

// errDependencyLost is what a publish returns when the suite is simulating a
// broker or pipeline that is refusing everything.
var errDependencyLost = errors.New("conformance: dependency lost")

// connectorFixture is one connector wired to a live local upstream.
type connectorFixture struct {
	// source is opened by the suite, not by the fixture: several assertions
	// are about Open itself.
	source StreamSource

	// emit makes exactly one well-formed event reach publish. It may be
	// called only once the session has reported ready.
	emit func(t *testing.T)

	// emitMalformed does the same with a frame that is not valid JSON. Every
	// connector has to survive one and produce an event the broker can still
	// encode.
	emitMalformed func(t *testing.T)

	// loseUpstream takes the far end away mid-session: a closed connection, a
	// handler that returns, a server that stops listening.
	loseUpstream func(t *testing.T)

	// sessions reports how many times the upstream has been reached. It is
	// what makes "a source does not retry" an assertion rather than a hope.
	sessions func() int
}

// freePort returns a port nothing is listening on, having released it. It is
// used where a source must be re-openable on the same address, which port 0
// cannot express: the OS would pick a different port the second time and the
// assertion would pass without the listener ever having been released.
func freePort(t *testing.T) string {
	t.Helper()

	lis, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	_, port, err := net.SplitHostPort(lis.Addr().String())
	if err != nil {
		_ = lis.Close()
		t.Fatalf("split addr: %v", err)
	}
	if err := lis.Close(); err != nil {
		t.Fatalf("release port: %v", err)
	}
	return port
}

// deadAddress returns a host:port that refuses connections: a port reserved
// and released, so a dial to it fails immediately rather than hanging.
func deadAddress(t *testing.T) string {
	t.Helper()
	return "127.0.0.1:" + freePort(t)
}

// --- WebSocket ---------------------------------------------------------

// wsUpstream is a WebSocket server that hands each accepted connection back to
// the test, so a fault can be injected on the server side of a live session.
type wsUpstream struct {
	server   *httptest.Server
	accepted atomic.Int64

	conns chan *websocket.Conn

	mu      sync.Mutex
	held    []*websocket.Conn
	current *websocket.Conn
}

func newWSUpstream(t *testing.T) *wsUpstream {
	t.Helper()

	up := &wsUpstream{conns: make(chan *websocket.Conn, 4)}
	upgrader := websocket.Upgrader{}

	up.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		up.accepted.Add(1)
		up.mu.Lock()
		up.held = append(up.held, conn)
		up.mu.Unlock()
		up.conns <- conn

		// Hold the connection open until one side closes it. Reading is how
		// the handler notices, and it also drains anything on_connect_send
		// puts on the wire.
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))

	t.Cleanup(func() {
		up.mu.Lock()
		for _, conn := range up.held {
			_ = conn.Close()
		}
		up.mu.Unlock()
		up.server.Close()
	})
	return up
}

// conn returns the server side of the live session, waiting for the handshake
// if it has not landed yet, and remembering it so later calls within the same
// session get the same connection.
func (up *wsUpstream) conn(t *testing.T) *websocket.Conn {
	t.Helper()

	up.mu.Lock()
	current := up.current
	up.mu.Unlock()
	if current != nil {
		return current
	}

	select {
	case conn := <-up.conns:
		up.mu.Lock()
		up.current = conn
		up.mu.Unlock()
		return conn
	case <-time.After(5 * time.Second):
		t.Fatal("websocket upstream: no connection accepted")
		return nil
	}
}

func (up *wsUpstream) url() string {
	return "ws" + up.server.URL[len("http"):]
}

func websocketFixture(t *testing.T) connectorFixture {
	t.Helper()

	up := newWSUpstream(t)
	src := NewWebSocketSource("conformance-websocket", up.url(), "nephtys.stream.conformance", nil)

	write := func(t *testing.T, payload string) {
		t.Helper()
		if err := up.conn(t).WriteMessage(websocket.TextMessage, []byte(payload)); err != nil {
			t.Fatalf("websocket upstream write: %v", err)
		}
	}

	return connectorFixture{
		source:        src,
		emit:          func(t *testing.T) { t.Helper(); write(t, `{"e":"reading","value":1}`) },
		emitMalformed: func(t *testing.T) { t.Helper(); write(t, malformedFrame) },
		loseUpstream: func(t *testing.T) {
			t.Helper()
			if err := up.conn(t).Close(); err != nil {
				t.Fatalf("websocket upstream close: %v", err)
			}
		},
		sessions: func() int { return int(up.accepted.Load()) },
	}
}

func websocketDetached(t *testing.T) StreamSource {
	t.Helper()
	return NewWebSocketSource("detached-websocket", "ws://"+deadAddress(t)+"/stream", "topic", nil)
}

// --- SSE ---------------------------------------------------------------

// sseUpstream is an event-stream server whose frames the test writes, and
// whose response the test can end.
type sseUpstream struct {
	server   *httptest.Server
	accepted atomic.Int64

	frames chan string
	stop   chan struct{}
	once   sync.Once
}

func newSSEUpstream(t *testing.T) *sseUpstream {
	t.Helper()

	up := &sseUpstream{
		frames: make(chan string, 8),
		stop:   make(chan struct{}),
	}

	up.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up.accepted.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}
		flusher.Flush()

		for {
			select {
			case frame := <-up.frames:
				if _, err := fmt.Fprint(w, frame); err != nil {
					return
				}
				flusher.Flush()
			case <-up.stop:
				return
			case <-r.Context().Done():
				return
			}
		}
	}))

	t.Cleanup(func() {
		up.once.Do(func() { close(up.stop) })
		up.server.Close()
	})
	return up
}

func sseFixture(t *testing.T) connectorFixture {
	t.Helper()

	up := newSSEUpstream(t)
	src := NewSSESource("conformance-sse", up.server.URL, "nephtys.stream.conformance", nil)

	send := func(t *testing.T, frame string) {
		t.Helper()
		select {
		case up.frames <- frame:
		case <-time.After(5 * time.Second):
			t.Fatal("sse upstream: frame queue never drained")
		}
	}

	return connectorFixture{
		source:        src,
		emit:          func(t *testing.T) { t.Helper(); send(t, "event: reading\ndata: "+wellFormedFrame+"\n\n") },
		emitMalformed: func(t *testing.T) { t.Helper(); send(t, "event: reading\ndata: "+malformedFrame+"\n\n") },
		loseUpstream: func(t *testing.T) {
			t.Helper()
			up.once.Do(func() { close(up.stop) })
		},
		sessions: func() int { return int(up.accepted.Load()) },
	}
}

func sseDetached(t *testing.T) StreamSource {
	t.Helper()
	return NewSSESource("detached-sse", "http://"+deadAddress(t)+"/events", "topic", nil)
}

// --- REST poller -------------------------------------------------------

// pollInterval is short enough that the suite never waits on a tick, and long
// enough that a session under test is not dominated by polling.
const pollInterval = 20 * time.Millisecond

type restUpstream struct {
	server *httptest.Server
	polls  atomic.Int64

	body atomic.Value // string
}

func newRESTUpstream(t *testing.T) *restUpstream {
	t.Helper()

	up := &restUpstream{}
	up.body.Store(wellFormedFrame)
	up.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		up.polls.Add(1)
		body, _ := up.body.Load().(string)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(up.server.Close)
	return up
}

func restPollerFixture(t *testing.T) connectorFixture {
	t.Helper()

	up := newRESTUpstream(t)
	src := NewRESTPollerSource("conformance-rest", up.server.URL, "nephtys.stream.conformance",
		&domain.RestPollerConfig{Interval: pollInterval.String(), Method: http.MethodGet})

	return connectorFixture{
		source: src,
		// A poller emits on its own schedule: it polls once on entry and then
		// on every tick, so there is nothing to trigger. Setting the body is
		// what decides which of the two kinds of event the next tick carries.
		emit:          func(t *testing.T) { t.Helper(); up.body.Store(wellFormedFrame) },
		emitMalformed: func(t *testing.T) { t.Helper(); up.body.Store(malformedFrame) },
		loseUpstream:  func(t *testing.T) { t.Helper(); up.server.Close() },
		sessions:      func() int { return int(up.polls.Load()) },
	}
}

func restPollerDetached(t *testing.T) StreamSource {
	t.Helper()
	return NewRESTPollerSource("detached-rest", "http://"+deadAddress(t)+"/data", "topic",
		&domain.RestPollerConfig{Interval: pollInterval.String()})
}

// crlf is the HTTP line terminator, named so the hand-written request below
// does not have to carry escapes through this file.
const crlf = "\r\n"

// --- Webhook -----------------------------------------------------------

// inboundUpstream stands in for the far end of a push connector. There is no
// remote host to lose: the "upstream" is whatever client posts to the bound
// listener, and losing it is a client going away, which a push source is
// contractually indifferent to.
type inboundUpstream struct {
	addr     string
	requests atomic.Int64
}

func webhookFixture(t *testing.T) connectorFixture {
	t.Helper()

	port := freePort(t)
	up := &inboundUpstream{addr: "http://127.0.0.1:" + port + "/hook"}
	src := NewWebhookSource("conformance-webhook", "nephtys.stream.conformance",
		&domain.WebhookConfig{Port: port, Path: "/hook"})

	post := func(t *testing.T, body string) {
		t.Helper()
		up.requests.Add(1)
		req, err := http.NewRequest(http.MethodPost, up.addr, strings.NewReader(body))
		if err != nil {
			t.Fatalf("build webhook request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("post to webhook: %v", err)
		}
		_ = resp.Body.Close()
	}

	return connectorFixture{
		source:        src,
		emit:          func(t *testing.T) { t.Helper(); post(t, wellFormedFrame) },
		emitMalformed: func(t *testing.T) { t.Helper(); post(t, malformedFrame) },
		// A push connector has no upstream connection of its own, so the
		// nearest equivalent fault is a client that dies part-way through a
		// request: headers promising a body, then a socket that goes away.
		loseUpstream: func(t *testing.T) {
			t.Helper()
			conn, err := net.Dial("tcp", "127.0.0.1:"+port)
			if err != nil {
				t.Fatalf("dial webhook: %v", err)
			}
			request := "POST /hook HTTP/1.1" + crlf + "Host: localhost" + crlf +
				"Content-Length: 64" + crlf + crlf + malformedFrame
			_, _ = conn.Write([]byte(request))
			if err := conn.Close(); err != nil {
				t.Fatalf("abandon webhook request: %v", err)
			}
		},
		sessions: func() int { return int(up.requests.Load()) },
	}
}

func webhookDetached(t *testing.T) StreamSource {
	t.Helper()
	return NewWebhookSource("detached-webhook", "topic", &domain.WebhookConfig{Port: freePort(t), Path: "/hook"})
}

// --- gRPC --------------------------------------------------------------

func grpcFixture(t *testing.T) connectorFixture {
	t.Helper()

	port := freePort(t)
	up := &inboundUpstream{addr: "127.0.0.1:" + port}
	src := NewGrpcSource("conformance-grpc", "nephtys.stream.conformance", &domain.GrpcConfig{Port: port})

	send := func(t *testing.T, payload string) {
		t.Helper()
		up.requests.Add(1)

		conn, err := grpc.NewClient(up.addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			t.Fatalf("grpc dial: %v", err)
		}
		defer func() { _ = conn.Close() }()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		stream, err := pb.NewStreamerClient(conn).StreamEvents(ctx)
		if err != nil {
			t.Fatalf("open grpc stream: %v", err)
		}
		if err := stream.Send(&pb.IngestRequest{Type: "reading", Payload: []byte(payload)}); err != nil {
			t.Fatalf("grpc send: %v", err)
		}
		// The response is deliberately not asserted on: a publish that fails
		// ends the client's stream by design, and this fixture is used by the
		// case that makes every publish fail.
		_, _ = stream.CloseAndRecv()
	}

	return connectorFixture{
		source:        src,
		emit:          func(t *testing.T) { t.Helper(); send(t, wellFormedFrame) },
		emitMalformed: func(t *testing.T) { t.Helper(); send(t, malformedFrame) },
		// The gRPC equivalent: a client that opens a stream, sends, and
		// vanishes without closing it. The server's Recv fails, that client's
		// stream ends, and the session serving it must not.
		loseUpstream: func(t *testing.T) {
			t.Helper()
			conn, err := grpc.NewClient(up.addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				t.Fatalf("grpc dial: %v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			stream, err := pb.NewStreamerClient(conn).StreamEvents(ctx)
			if err != nil {
				t.Fatalf("open grpc stream: %v", err)
			}
			if err := stream.Send(&pb.IngestRequest{Type: "reading", Payload: []byte(wellFormedFrame)}); err != nil {
				t.Fatalf("grpc send: %v", err)
			}
			if err := conn.Close(); err != nil {
				t.Fatalf("abandon grpc stream: %v", err)
			}
		},
		sessions: func() int { return int(up.requests.Load()) },
	}
}

func grpcDetached(t *testing.T) StreamSource {
	t.Helper()
	return NewGrpcSource("detached-grpc", "topic", &domain.GrpcConfig{Port: freePort(t)})
}
