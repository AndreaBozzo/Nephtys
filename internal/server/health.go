package server

import (
	"net/http"
)

// brokerHealth is the readiness dependency: the broker connection whose loss
// stops the instance accepting and serving streams. It is an interface, not
// *broker.Broker, so a probe test can present a disconnected dependency
// without standing up a NATS server to break.
type brokerHealth interface {
	IsConnected() bool
	ConnState() string
	JetStreamAvailable() bool
}

// Probe response literals. Both endpoints answer from a closed set of strings:
// the broker URL routinely carries credentials and a NATS error quotes it, so a
// probe reports which dependency is not ready and in what state, never what the
// dependency is called or what the client said about it. Nothing an operator
// typed can reach these responses.
const (
	statusAlive   = "alive"
	statusReady   = "ready"
	statusUnready = "unready"

	checkOK          = "ok"
	checkUnavailable = "unavailable"
	checkUnknown     = "unknown"

	reasonBrokerUnavailable    = "broker connection is not established"
	reasonJetStreamUnavailable = "jetstream is not available on the broker connection"
)

// handleLivez reports that the process is running and its HTTP server is
// answering. It checks no dependency on purpose: a liveness probe that fails
// during a broker outage restarts every instance in the deployment for a fault
// none of them can fix, which is why /health — which does look at the broker —
// is the wrong endpoint to point a liveness probe at.
func (s *Server) handleLivez(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": statusAlive})
}

// handleReadyz reports whether the instance can accept and manage streams,
// which takes two things and not one: a live broker connection, and JetStream
// answering on it. Registration writes the config to the JetStream KV bucket
// and every accepted event is published to JetStream, so an instance that has
// one without the other can serve reads and nothing else.
//
// The two are checked in order and the JetStream round trip is skipped when the
// connection is down, where it could only time out. That check is then reported
// as "unknown" rather than as a failure: the probe did not ask, and a readiness
// body that claims a dependency failed when it was never consulted is how an
// operator ends up debugging the wrong one.
//
// 200 means ready, 503 means not ready yet or not ready any more; a 503 is a
// signal to stop routing to this instance, not to restart it. The connection
// reconnects indefinitely, so readiness returns on its own once the broker is
// back.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	brokerOK := s.broker.IsConnected()

	jetStreamStatus := checkUnknown
	jetStreamOK := false
	if brokerOK {
		jetStreamOK = s.broker.JetStreamAvailable()
		jetStreamStatus = checkStatus(jetStreamOK)
	}

	checks := map[string]any{
		"broker": map[string]any{
			"status": checkStatus(brokerOK),
			"state":  s.broker.ConnState(),
		},
		"jetstream": map[string]any{
			"status": jetStreamStatus,
		},
	}

	if reason := unreadyReason(brokerOK, jetStreamOK); reason != "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status": statusUnready,
			"reason": reason,
			"checks": checks,
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status": statusReady,
		"checks": checks,
	})
}

// unreadyReason names the first dependency that is not ready, or "" when both
// are. The order matters: a broker that is down explains a JetStream check that
// never ran, and reporting the derived failure would send an operator after the
// wrong dependency.
func unreadyReason(brokerOK, jetStreamOK bool) string {
	switch {
	case !brokerOK:
		return reasonBrokerUnavailable
	case !jetStreamOK:
		return reasonJetStreamUnavailable
	default:
		return ""
	}
}

func checkStatus(ok bool) string {
	if ok {
		return checkOK
	}
	return checkUnavailable
}
