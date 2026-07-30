package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nephtys/internal/domain"
)

// validSSE returns a minimal stream config that passes validation, so each test
// below varies exactly one thing.
func validSSE() domain.StreamSourceConfig {
	return domain.StreamSourceConfig{
		ID:    "sensor-1",
		Kind:  "sse",
		URL:   "https://example.test/stream",
		Topic: "nephtys.stream.sensors.one",
	}
}

func TestValidateStreamConfig_Accepts(t *testing.T) {
	tests := []struct {
		name string
		cfg  domain.StreamSourceConfig
	}{
		{"sse", validSSE()},
		{
			"websocket with connect frames",
			domain.StreamSourceConfig{
				ID: "ws-1", Kind: "websocket", URL: "wss://example.test/ws", Topic: "t",
				Websocket: &domain.WebsocketConfig{OnConnectSend: domain.StringList{"{}"}},
			},
		},
		{
			"rest_poller with interval",
			domain.StreamSourceConfig{
				ID: "poll-1", Kind: "rest_poller", URL: "https://example.test/api", Topic: "t",
				RestPoller: &domain.RestPollerConfig{Interval: "30s", Method: "GET"},
			},
		},
		{
			// NewRESTPollerSource defaults these to 1m and GET, so an omitted
			// value is "no opinion" rather than a malformed one.
			"rest_poller with an omitted interval and method",
			domain.StreamSourceConfig{
				ID: "poll-2", Kind: "rest_poller", URL: "https://example.test/api", Topic: "t",
				RestPoller: &domain.RestPollerConfig{},
			},
		},
		{
			"webhook needs no url",
			domain.StreamSourceConfig{
				ID: "hook-1", Kind: "webhook", Topic: "t",
				Webhook: &domain.WebhookConfig{Port: "8081", Path: "/hook"},
			},
		},
		{
			"grpc needs no url",
			domain.StreamSourceConfig{
				ID: "grpc-1", Kind: "grpc", Topic: "t",
				Grpc: &domain.GrpcConfig{Port: "50051"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := validateStreamConfig(tt.cfg); err != nil {
				t.Errorf("validateStreamConfig() = %v, want nil", err)
			}
		})
	}
}

func TestValidateStreamConfig_Rejects(t *testing.T) {
	withKind := func(kind string) domain.StreamSourceConfig {
		cfg := validSSE()
		cfg.Kind = kind
		return cfg
	}

	tests := []struct {
		name     string
		cfg      domain.StreamSourceConfig
		wantPath string
	}{
		{"missing id", func() domain.StreamSourceConfig { c := validSSE(); c.ID = ""; return c }(), "id"},
		{"id with a slash", func() domain.StreamSourceConfig { c := validSSE(); c.ID = "a/b"; return c }(), "id"},
		{"missing kind", withKind(""), "kind"},
		{"unsupported kind", withKind("kafka"), "kind"},
		{"missing topic", func() domain.StreamSourceConfig { c := validSSE(); c.Topic = ""; return c }(), "topic"},
		{"topic with a wildcard", func() domain.StreamSourceConfig { c := validSSE(); c.Topic = "a.*"; return c }(), "topic"},
		{"missing url on a pull kind", func() domain.StreamSourceConfig { c := validSSE(); c.URL = ""; return c }(), "url"},
		{"url without a host", func() domain.StreamSourceConfig { c := validSSE(); c.URL = "https://"; return c }(), "url"},
		{"wrong scheme for sse", func() domain.StreamSourceConfig { c := validSSE(); c.URL = "ws://x/y"; return c }(), "url"},
		{
			"wrong scheme for websocket",
			domain.StreamSourceConfig{ID: "a", Kind: "websocket", URL: "https://x/y", Topic: "t"},
			"url",
		},
		{
			// The connector reads only its own block, so a block for a different
			// kind is configuration Nephtys would never apply.
			"connector block for a different kind",
			func() domain.StreamSourceConfig {
				c := validSSE()
				c.Websocket = &domain.WebsocketConfig{OnConnectSend: domain.StringList{"{}"}}
				return c
			}(),
			"websocket",
		},
		{
			"empty websocket connect frame",
			domain.StreamSourceConfig{
				ID: "a", Kind: "websocket", URL: "wss://x/y", Topic: "t",
				Websocket: &domain.WebsocketConfig{OnConnectSend: domain.StringList{""}},
			},
			"websocket.on_connect_send[0]",
		},
		{
			"lower-cased rest_poller method",
			domain.StreamSourceConfig{
				ID: "a", Kind: "rest_poller", URL: "https://x/y", Topic: "t",
				RestPoller: &domain.RestPollerConfig{Interval: "30s", Method: "get"},
			},
			"rest_poller.method",
		},
		{
			"unknown rest_poller method",
			domain.StreamSourceConfig{
				ID: "a", Kind: "rest_poller", URL: "https://x/y", Topic: "t",
				RestPoller: &domain.RestPollerConfig{Interval: "30s", Method: "FETCH"},
			},
			"rest_poller.method",
		},
		{
			// Validated here rather than in Start(), where it used to fail after
			// the API had already answered 201 Created.
			"unparseable rest_poller interval",
			domain.StreamSourceConfig{
				ID: "a", Kind: "rest_poller", URL: "https://x/y", Topic: "t",
				RestPoller: &domain.RestPollerConfig{Interval: "every 5s"},
			},
			"rest_poller.interval",
		},
		{
			"unitless rest_poller interval",
			domain.StreamSourceConfig{
				ID: "a", Kind: "rest_poller", URL: "https://x/y", Topic: "t",
				RestPoller: &domain.RestPollerConfig{Interval: "60"},
			},
			"rest_poller.interval",
		},
		{
			"rest_poller interval below the floor",
			domain.StreamSourceConfig{
				ID: "a", Kind: "rest_poller", URL: "https://x/y", Topic: "t",
				RestPoller: &domain.RestPollerConfig{Interval: "1ms"},
			},
			"rest_poller.interval",
		},
		{
			"webhook port out of range",
			domain.StreamSourceConfig{
				ID: "a", Kind: "webhook", Topic: "t",
				Webhook: &domain.WebhookConfig{Port: "99999"},
			},
			"webhook.port",
		},
		{
			"non-numeric grpc port",
			domain.StreamSourceConfig{
				ID: "a", Kind: "grpc", Topic: "t",
				Grpc: &domain.GrpcConfig{Port: "fifty"},
			},
			"grpc.port",
		},
		{
			// Delegated to pipeline.ValidateConfig; asserted here so the wiring
			// itself is covered, not just the pipeline package.
			"invalid pipeline is rejected through the stream validator",
			func() domain.StreamSourceConfig {
				c := validSSE()
				c.Pipeline = &domain.PipelineConfig{Batch: &domain.BatchConfig{Enabled: true, FlushInterval: "1 sec"}}
				return c
			}(),
			"pipeline.batch.flush_interval",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateStreamConfig(tt.cfg)
			if err == nil {
				t.Fatalf("validateStreamConfig() = nil, want an error naming %s", tt.wantPath)
			}
			if !strings.Contains(err.Error(), tt.wantPath) {
				t.Errorf("error %q does not name the offending path %q", err, tt.wantPath)
			}
		})
	}
}

func TestDecodeStreamConfig_Strict(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr string // substring; empty means the document must decode
	}{
		{
			"valid document",
			`{"id":"a","kind":"sse","url":"https://x/y","topic":"t"}`,
			"",
		},
		{
			// The mistake this whole path exists for: a misspelled field used to
			// be dropped, leaving the default silently in force.
			"misspelled top-level field",
			`{"id":"a","kind":"sse","url":"https://x/y","topic":"t","websockets":{}}`,
			"websockets",
		},
		{
			"misspelled nested field",
			`{"id":"a","kind":"sse","url":"https://x/y","topic":"t","pipeline":{"batch":{"flush_intervl":"5m"}}}`,
			"flush_intervl",
		},
		{
			"unknown middleware",
			`{"id":"a","kind":"sse","url":"https://x/y","topic":"t","pipeline":{"decimate":{}}}`,
			"decimate",
		},
		{
			"trailing content after the object",
			`{"id":"a","kind":"sse","url":"https://x/y","topic":"t"} {"extra":1}`,
			"unexpected content",
		},
		{
			"not JSON at all",
			`not json`,
			"invalid JSON",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DecodeStreamConfig(strings.NewReader(tt.body))
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("DecodeStreamConfig() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("DecodeStreamConfig() = nil, want an error mentioning %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestDecodePipelineConfig_Strict(t *testing.T) {
	if _, err := DecodePipelineConfig(strings.NewReader(`{"dedup":{"enabled":true,"tll":"30s"}}`)); err == nil {
		t.Error("DecodePipelineConfig accepted a misspelled field")
	}
	if _, err := DecodePipelineConfig(strings.NewReader(`{"dedup":{"enabled":true,"ttl":"30s"}}`)); err != nil {
		t.Errorf("DecodePipelineConfig rejected a valid body: %v", err)
	}
}

// TestSupportedKindsMatchSourceFromConfig keeps the validator's notion of a
// supported kind and the manager's constructor switch from drifting apart. A
// kind accepted here but unknown to sourceFromConfig would pass
// `--config-check` and then fail at registration.
func TestSupportedKindsMatchSourceFromConfig(t *testing.T) {
	for kind := range connectorBlocks {
		cfg := domain.StreamSourceConfig{ID: "a", Kind: kind, Topic: "t"}
		if _, err := sourceFromConfig(cfg); err != nil {
			t.Errorf("kind %q is validated as supported but sourceFromConfig rejects it: %v", kind, err)
		}
	}

	if _, err := sourceFromConfig(domain.StreamSourceConfig{ID: "a", Kind: "kafka", Topic: "t"}); err == nil {
		t.Error("sourceFromConfig accepted a kind the validator rejects")
	}
}

// TestShippedExamplesAreValid runs the published example configs through the
// exact decode-and-validate path the REST API uses. `make check-examples` covers
// the same ground via the CLI; this keeps the guarantee inside `go test`, where
// a validation change is made.
func TestShippedExamplesAreValid(t *testing.T) {
	dir := filepath.Join("..", "..", "docs", "examples")
	entries, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		t.Fatalf("glob examples: %v", err)
	}
	if len(entries) == 0 {
		t.Fatalf("no example configs found in %s — did the directory move?", dir)
	}

	for _, path := range entries {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			cfg, err := DecodeStreamConfig(strings.NewReader(string(data)))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if err := validateStreamConfig(cfg); err != nil {
				t.Fatalf("validate: %v", err)
			}
		})
	}
}
