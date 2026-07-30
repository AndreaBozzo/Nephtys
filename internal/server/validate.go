package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"nephtys/internal/domain"
	"nephtys/internal/pipeline"
)

// topicPattern matches NATS publish-safe subject characters (no wildcards).
var topicPattern = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// idPattern keeps stream IDs usable as JetStream KV keys and URL path segments.
var idPattern = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// minPollInterval floors rest_poller.interval. A sub-100ms poll is not a
// workload any REST source supports; in practice it means the author wrote a
// unit they did not intend.
const minPollInterval = 100 * time.Millisecond

// connectorBlocks maps each kind to the name of the connector config block it
// reads, so a block belonging to a different kind can be reported rather than
// silently ignored. sourceFromConfig must handle exactly these kinds — see
// TestSupportedKindsMatchSourceFromConfig.
var connectorBlocks = map[string]string{
	"websocket":   "websocket",
	"sse":         "sse",
	"rest_poller": "rest_poller",
	"webhook":     "webhook",
	"grpc":        "grpc",
}

// DecodeStreamConfig strictly decodes a stream configuration from r.
//
// Strict means two things beyond ordinary decoding: an unknown or misspelled
// field is an error rather than being dropped, and content after the JSON value
// is an error rather than being ignored. Both matter for a service whose entire
// behavior is JSON-configured — "flush_intervl" silently running the default is
// exactly the class of mistake an unattended edge deployment cannot afford.
//
// This is the single decode path for both `--config-check` and
// POST /v1/streams, so the CLI cannot accept a document the API would reject.
func DecodeStreamConfig(r io.Reader) (domain.StreamSourceConfig, error) {
	var cfg domain.StreamSourceConfig
	if err := decodeStrict(r, &cfg); err != nil {
		return domain.StreamSourceConfig{}, err
	}
	return cfg, nil
}

// DecodePipelineConfig strictly decodes a pipeline configuration from r, with
// the same rules as DecodeStreamConfig.
func DecodePipelineConfig(r io.Reader) (domain.PipelineConfig, error) {
	var cfg domain.PipelineConfig
	if err := decodeStrict(r, &cfg); err != nil {
		return domain.PipelineConfig{}, err
	}
	return cfg, nil
}

func decodeStrict(r io.Reader, v any) error {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	// A second Decode is how a trailing value is detected: io.EOF here means the
	// document held exactly one JSON value, which is what a config must be.
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return fmt.Errorf("invalid JSON: unexpected content after the configuration object")
	}
	return nil
}

// ValidateStreamConfig performs input validation on a stream configuration.
// Exposed for use by `nephtys --config-check`, which must apply exactly the
// rules POST /v1/streams applies — a config the CLI calls valid has to be one
// the running service accepts.
func ValidateStreamConfig(cfg domain.StreamSourceConfig) error {
	return validateStreamConfig(cfg)
}

// validateStreamConfig performs input validation on a stream configuration.
func validateStreamConfig(cfg domain.StreamSourceConfig) error {
	if cfg.ID == "" {
		return fmt.Errorf("id: required")
	}
	if !idPattern.MatchString(cfg.ID) {
		return fmt.Errorf("id: invalid %q: must match [a-zA-Z0-9._-]+", cfg.ID)
	}
	if cfg.Kind == "" {
		return fmt.Errorf("kind: required")
	}
	if _, ok := connectorBlocks[cfg.Kind]; !ok {
		return fmt.Errorf("kind: unsupported %q (want one of %s)", cfg.Kind, supportedKindList())
	}
	if cfg.Topic == "" {
		return fmt.Errorf("topic: required")
	}
	if !topicPattern.MatchString(cfg.Topic) {
		return fmt.Errorf("topic: invalid %q: must match [a-zA-Z0-9._-]+", cfg.Topic)
	}

	if err := validateConnector(cfg); err != nil {
		return err
	}

	return pipeline.ValidateConfig(cfg.Pipeline)
}

// validateConnector checks the URL and the per-kind connector block. A block
// belonging to a different kind is rejected rather than ignored: the connector
// reads only its own block, so a `websocket` block on an `sse` stream is
// configuration the operator wrote and Nephtys would never apply.
func validateConnector(cfg domain.StreamSourceConfig) error {
	// URL is required for the pull connectors and meaningless for the push ones.
	pushKind := cfg.Kind == "webhook" || cfg.Kind == "grpc"
	if !pushKind {
		if cfg.URL == "" {
			return fmt.Errorf("url: required for kind %q", cfg.Kind)
		}
		u, err := url.Parse(cfg.URL)
		if err != nil {
			return fmt.Errorf("url: invalid: %w", err)
		}
		if u.Host == "" {
			return fmt.Errorf("url: must include a host")
		}
		switch cfg.Kind {
		case "websocket":
			if u.Scheme != "ws" && u.Scheme != "wss" {
				return fmt.Errorf("url: websocket url must use the ws:// or wss:// scheme")
			}
		case "rest_poller", "sse":
			if u.Scheme != "http" && u.Scheme != "https" {
				return fmt.Errorf("url: %s url must use the http:// or https:// scheme", cfg.Kind)
			}
		}
	}

	present := presentConnectorBlocks(cfg)
	for _, block := range present {
		if block != connectorBlocks[cfg.Kind] {
			return fmt.Errorf("%s: block does not apply to kind %q and would be ignored", block, cfg.Kind)
		}
	}

	if ws := cfg.Websocket; ws != nil {
		for i, msg := range ws.OnConnectSend {
			if msg == "" {
				return fmt.Errorf("websocket.on_connect_send[%d]: must not be empty", i)
			}
		}
	}

	if rp := cfg.RestPoller; rp != nil {
		if err := validatePollInterval(rp.Interval); err != nil {
			return err
		}
		if err := validatePollMethod(rp.Method); err != nil {
			return err
		}
		if err := validateHeaders("rest_poller.headers", rp.Headers); err != nil {
			return err
		}
	}

	if sse := cfg.Sse; sse != nil {
		if err := validateHeaders("sse.headers", sse.Headers); err != nil {
			return err
		}
	}

	if wh := cfg.Webhook; wh != nil && wh.Port != "" {
		if err := validatePort("webhook.port", wh.Port); err != nil {
			return err
		}
	}
	if g := cfg.Grpc; g != nil && g.Port != "" {
		if err := validatePort("grpc.port", g.Port); err != nil {
			return err
		}
	}

	return nil
}

// validatePollInterval checks rest_poller.interval at config time. Unparseable
// values used to surface only from RESTPollerSource.Start, which runs after the
// API has already answered 201 Created — the stream registered successfully and
// the connector died immediately afterwards.
//
// An omitted interval is left alone: NewRESTPollerSource defaults it to 1m, and
// absent means "no opinion" here as it does everywhere else in the contract.
func validatePollInterval(raw string) error {
	if raw == "" {
		return nil
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("rest_poller.interval: %q is not a valid duration (want a unit suffix, e.g. \"30s\", \"5m\")", raw)
	}
	if parsed < minPollInterval {
		return fmt.Errorf("rest_poller.interval: must be at least %s, got %q", minPollInterval, raw)
	}
	return nil
}

// pollMethods are the HTTP methods a REST poller may use. Go sends a method
// verbatim and does not upper-case it, so "get" is a different request from
// "GET" and many servers reject it — a lower-cased method is a typo worth
// catching rather than a preference worth honouring.
var pollMethods = map[string]bool{
	http.MethodGet:     true,
	http.MethodPost:    true,
	http.MethodPut:     true,
	http.MethodPatch:   true,
	http.MethodDelete:  true,
	http.MethodHead:    true,
	http.MethodOptions: true,
}

func validatePollMethod(raw string) error {
	if raw == "" {
		return nil // NewRESTPollerSource defaults to GET.
	}
	if !pollMethods[raw] {
		return fmt.Errorf("rest_poller.method: %q is not a supported HTTP method (want an upper-case method such as GET or POST)", raw)
	}
	return nil
}

func validateHeaders(path string, headers map[string]string) error {
	if _, ok := headers[""]; ok {
		return fmt.Errorf("%s: header names must not be empty", path)
	}
	return nil
}

func validatePort(path, s string) error {
	port, err := strconv.Atoi(s)
	if err != nil {
		return fmt.Errorf("%s: %q is not a valid port number", path, s)
	}
	if port < 1 || port > 65535 {
		return fmt.Errorf("%s: %d out of range (1-65535)", path, port)
	}
	return nil
}

// presentConnectorBlocks lists the connector blocks set on cfg, in a stable
// order so an error message does not depend on map iteration.
func presentConnectorBlocks(cfg domain.StreamSourceConfig) []string {
	var present []string
	if cfg.Websocket != nil {
		present = append(present, "websocket")
	}
	if cfg.Sse != nil {
		present = append(present, "sse")
	}
	if cfg.RestPoller != nil {
		present = append(present, "rest_poller")
	}
	if cfg.Webhook != nil {
		present = append(present, "webhook")
	}
	if cfg.Grpc != nil {
		present = append(present, "grpc")
	}
	return present
}

func supportedKindList() string {
	// Stable order, independent of map iteration.
	return "grpc, rest_poller, sse, webhook, websocket"
}
