package pipeline

import (
	"encoding/json"
	"testing"
	"time"

	"nephtys/internal/domain"
)

func TestFilterMiddleware(t *testing.T) {
	cfg := &domain.FilterConfig{
		MatchTypes: []string{"trade", "ticker"},
	}
	filter := NewFilter(cfg)

	tests := []struct {
		name      string
		eventType string
		wantOk    bool
	}{
		{"match trade", "trade", true},
		{"match ticker", "ticker", true},
		{"no match unknown", "unknown", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			evt := domain.StreamEvent{Type: tt.eventType}
			_, ok := filter(evt)
			if ok != tt.wantOk {
				t.Errorf("filter() = %v, want %v", ok, tt.wantOk)
			}
		})
	}
}

func TestEnrichMiddleware(t *testing.T) {
	cfg := &domain.EnrichConfig{
		Tags: map[string]string{"env": "prod", "version": "1.0"},
	}
	enrich := NewEnrich(cfg)

	// Valid JSON object
	evt := domain.StreamEvent{
		Payload: json.RawMessage(`{"price": 100}`),
	}
	res, ok := enrich(evt)
	if !ok {
		t.Fatal("enrich() returned false")
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(res.Payload, &payload); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}

	if payload["env"] != "prod" || payload["version"] != "1.0" {
		t.Errorf("payload not enriched correctly: %v", payload)
	}
	if payload["price"] != float64(100) {
		t.Errorf("original payload data lost: %v", payload)
	}

	// Invalid JSON (should pass through unharmed)
	evtInvalid := domain.StreamEvent{
		Payload: json.RawMessage(`not-json`),
	}
	resInvalid, okInvalid := enrich(evtInvalid)
	if !okInvalid {
		t.Fatal("enrich() invalid json returned false")
	}
	if string(resInvalid.Payload) != "not-json" {
		t.Errorf("expected untouched payload, got: %s", string(resInvalid.Payload))
	}
}

func TestDedupMiddleware(t *testing.T) {
	cfg := &domain.DedupConfig{
		Enabled:   true,
		CacheSize: 10,
		TTL:       "50ms",
	}
	dedup := NewDedup(cfg)

	evt1 := domain.StreamEvent{Payload: json.RawMessage(`{"id": 1}`)}
	evt2 := domain.StreamEvent{Payload: json.RawMessage(`{"id": 2}`)}
	evt1Copy := domain.StreamEvent{Payload: json.RawMessage(`{"id": 1}`)}

	// 1. First time evt1 -> ok
	if _, ok := dedup(evt1); !ok {
		t.Error("dedup() First evt1 should be true")
	}

	// 2. Second time evt1 -> dropped
	if _, ok := dedup(evt1Copy); ok {
		t.Error("dedup() Duplicate evt1 should be false")
	}

	// 3. First time evt2 -> ok
	if _, ok := dedup(evt2); !ok {
		t.Error("dedup() First evt2 should be true")
	}

	// 4. Wait for TTL to expire
	time.Sleep(100 * time.Millisecond)

	// 5. Evt1 again -> ok (cache expired)
	if _, ok := dedup(evt1Copy); !ok {
		t.Error("dedup() Expired evt1 should be true")
	}
}

func TestTransformMiddleware(t *testing.T) {
	cfg := &domain.TransformConfig{
		Mapping: map[string]string{
			"price":  "data.kline.c",
			"symbol": "symbol",
		},
	}
	transform := NewTransform(cfg)

	evt := domain.StreamEvent{
		Payload: json.RawMessage(`{
			"symbol": "BTCUSDT",
			"data": {
				"kline": {
					"c": "45000"
				}
			},
			"ignored": "field"
		}`),
	}

	res, ok := transform(evt)
	if !ok {
		t.Fatal("transform() returned false")
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(res.Payload, &payload); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}

	if payload["price"] != "45000" || payload["symbol"] != "BTCUSDT" {
		t.Errorf("payload not transformed correctly: %v", payload)
	}
	if _, exists := payload["ignored"]; exists {
		t.Errorf("unmapped 'ignored' field was not stripped: %v", payload)
	}

	// Test missing path (should skip mapping that key silently but keep others)
	evtMissing := domain.StreamEvent{
		Payload: json.RawMessage(`{"symbol": "ETHUSDT"}`), // missing data.kline.c
	}
	resMissing, _ := transform(evtMissing)
	var payloadMissing map[string]interface{}
	json.Unmarshal(resMissing.Payload, &payloadMissing)

	if payloadMissing["symbol"] != "ETHUSDT" {
		t.Errorf("expected symbol ETHUSDT, got %v", payloadMissing["symbol"])
	}
	if _, exists := payloadMissing["price"]; exists {
		t.Errorf("expected price to be missing, got %v", payloadMissing["price"])
	}
}

func TestBuilderFullConfig(t *testing.T) {
	cfg := &domain.PipelineConfig{
		Filter: &domain.FilterConfig{MatchTypes: []string{"allowed"}},
		Enrich: &domain.EnrichConfig{Tags: map[string]string{"foo": "bar"}},
		Dedup:  &domain.DedupConfig{Enabled: true},
	}

	pipe := BuildFromConfig(cfg)

	// Since we can't easily introspect the pipeline's internal functions,
	// let's do a functional test of the whole chain.

	// 1. Event dropped by filter (wrong type)
	droppedEvt := domain.StreamEvent{Type: "blocked", Payload: json.RawMessage(`{}`)}
	if _, ok := pipe.Process(droppedEvt); ok {
		t.Error("expected event to be filtered")
	}

	// 2. Event passes filter, gets enriched, and dedup stores it
	goodEvt := domain.StreamEvent{Type: "allowed", Payload: json.RawMessage(`{"val": 1}`)}
	res, ok := pipe.Process(goodEvt)
	if !ok {
		t.Fatal("expected event to pass")
	}

	var payload map[string]interface{}
	json.Unmarshal(res.Payload, &payload)
	if payload["foo"] != "bar" {
		t.Error("event was not enriched")
	}

	// 3. Duplicate event passes filter, but dropped by dedup (because payload the same as original)
	// Wait, the payload hash for dedup is computed BEFORE enrichment!
	// Oh actually, BuildFromConfig order: Filter -> Dedup -> Enrich
	// So dedup checks `{"val": 1}` hash. It was saved as seen.
	// Now we send `{"val": 1}` again.
	if _, ok := pipe.Process(goodEvt); ok {
		t.Error("expected duplicate event to be dropped")
	}
}
