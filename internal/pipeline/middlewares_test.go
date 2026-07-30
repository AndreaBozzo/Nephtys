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
	filter := NewFilter("test", cfg)

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
			passed := false
			sink := func(topic string, e domain.StreamEvent) error {
				passed = true
				return nil
			}
			handler := filter(sink)
			_ = handler("topic", evt)

			if passed != tt.wantOk {
				t.Errorf("filter() ok = %v, want %v", passed, tt.wantOk)
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

	var res domain.StreamEvent
	sink := func(topic string, e domain.StreamEvent) error {
		res = e
		return nil
	}

	handler := enrich(sink)
	_ = handler("topic", evt)

	if res.Payload == nil {
		t.Fatal("enrich() returned nil payload")
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
	resInvalid := domain.StreamEvent{}
	sinkInvalid := func(topic string, e domain.StreamEvent) error {
		resInvalid = e
		return nil
	}
	handlerInvalid := enrich(sinkInvalid)
	_ = handlerInvalid("topic", evtInvalid)

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
	dedup := mustDedup(t, "test", cfg)

	evt1 := domain.StreamEvent{Payload: json.RawMessage(`{"id": 1}`)}
	evt2 := domain.StreamEvent{Payload: json.RawMessage(`{"id": 2}`)}
	evt1Copy := domain.StreamEvent{Payload: json.RawMessage(`{"id": 1}`)}

	passed := false
	sink := func(topic string, e domain.StreamEvent) error {
		passed = true
		return nil
	}
	handler := dedup(sink)

	// 1. First time evt1 -> ok
	passed = false
	_ = handler("topic", evt1)
	if !passed {
		t.Error("dedup() First evt1 should be true")
	}

	// 2. Second time evt1 -> dropped
	passed = false
	_ = handler("topic", evt1Copy)
	if passed {
		t.Error("dedup() Duplicate evt1 should be false")
	}

	// 3. First time evt2 -> ok
	passed = false
	_ = handler("topic", evt2)
	if !passed {
		t.Error("dedup() First evt2 should be true")
	}

	// 4. Wait for TTL to expire
	time.Sleep(100 * time.Millisecond)

	// 5. Evt1 again -> ok (cache expired)
	passed = false
	_ = handler("topic", evt1Copy)
	if !passed {
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

	var res domain.StreamEvent
	sink := func(topic string, e domain.StreamEvent) error {
		res = e
		return nil
	}
	handler := transform(sink)
	_ = handler("topic", evt)

	if res.Payload == nil {
		t.Fatal("transform() returned nil payload")
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

	// Test missing path
	evtMissing := domain.StreamEvent{
		Payload: json.RawMessage(`{"symbol": "ETHUSDT"}`),
	}
	resMissing := domain.StreamEvent{}
	sinkMissing := func(topic string, e domain.StreamEvent) error {
		resMissing = e
		return nil
	}
	handlerMissing := transform(sinkMissing)
	_ = handlerMissing("topic", evtMissing)

	var payloadMissing map[string]interface{}
	if err := json.Unmarshal(resMissing.Payload, &payloadMissing); err != nil {
		t.Fatalf("failed to unmarshal missing result: %v", err)
	}

	if payloadMissing["symbol"] != "ETHUSDT" {
		t.Errorf("expected symbol ETHUSDT, got %v", payloadMissing["symbol"])
	}
	if _, exists := payloadMissing["price"]; exists {
		t.Errorf("expected price to be missing, got %v", payloadMissing["price"])
	}
}

func TestTransformMiddleware_ArrayPath(t *testing.T) {
	cfg := &domain.TransformConfig{
		Mapping: map[string]string{
			"pm25": "0.sensordatavalues.1.value",
			"lat":  "0.location.latitude",
		},
	}
	transform := NewTransform(cfg)

	evt := domain.StreamEvent{
		Payload: json.RawMessage(`[
			{
				"location": {"latitude": "38.42"},
				"sensordatavalues": [
					{"value_type": "P1", "value": "18.2"},
					{"value_type": "P2", "value": "9.4"}
				]
			}
		]`),
	}

	var res domain.StreamEvent
	handler := transform(func(topic string, e domain.StreamEvent) error {
		res = e
		return nil
	})
	_ = handler("topic", evt)

	var payload map[string]interface{}
	if err := json.Unmarshal(res.Payload, &payload); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}
	if payload["pm25"] != "9.4" || payload["lat"] != "38.42" {
		t.Fatalf("payload not transformed from array path: %v", payload)
	}
}

func TestBuilderFullConfig(t *testing.T) {
	cfg := &domain.PipelineConfig{
		Filter: &domain.FilterConfig{MatchTypes: []string{"allowed"}},
		Enrich: &domain.EnrichConfig{Tags: map[string]string{"foo": "bar"}},
		Dedup:  &domain.DedupConfig{Enabled: true},
	}

	var res *domain.StreamEvent
	sink := func(topic string, e domain.StreamEvent) error {
		res = &e
		return nil
	}

	gen := mustGeneration(t, "test", cfg, sink)
	handler := gen.Publish

	// 1. Event dropped by filter (wrong type)
	droppedEvt := domain.StreamEvent{Type: "blocked", Payload: json.RawMessage(`{}`)}
	_ = handler("topic", droppedEvt)
	if res != nil {
		t.Error("expected event to be filtered")
	}

	// 2. Event passes filter, gets enriched, and dedup stores it
	goodEvt := domain.StreamEvent{Type: "allowed", Payload: json.RawMessage(`{"val": 1}`)}
	_ = handler("topic", goodEvt)
	if res == nil {
		t.Fatal("expected event to pass")
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(res.Payload, &payload); err != nil {
		t.Fatalf("failed to unmarshal enriched result: %v", err)
	}
	if payload["foo"] != "bar" {
		t.Error("event was not enriched")
	}

	// 3. Duplicate event passes filter, but dropped by dedup
	res = nil // Reset sink
	_ = handler("topic", goodEvt)
	if res != nil {
		t.Error("expected duplicate event to be dropped")
	}
}

// Binary events leave Payload nil and carry their bytes on Data. Hashing
// Payload alone gives every binary event the same hash, so all but the first
// are dropped as duplicates regardless of their actual content.
func TestDedupMiddleware_BinaryEventsHashTheirData(t *testing.T) {
	cfg := &domain.DedupConfig{Enabled: true, CacheSize: 10, TTL: "1h"}
	dedup := mustDedup(t, "test", cfg)

	var passed int
	handler := dedup(func(topic string, e domain.StreamEvent) error {
		passed++
		return nil
	})

	binary := func(data []byte) domain.StreamEvent {
		return domain.StreamEvent{
			Source:      "test",
			Type:        "websocket_binary",
			ContentType: domain.ContentTypeBinary,
			Data:        data,
		}
	}

	// Three frames with distinct bytes must all pass.
	for i, frame := range [][]byte{{0x01}, {0x02}, {0x01, 0x02}} {
		passed = 0
		_ = handler("topic", binary(frame))
		if passed != 1 {
			t.Errorf("distinct binary frame %d was dropped as a duplicate", i)
		}
	}

	// A genuine repeat still dedups.
	passed = 0
	_ = handler("topic", binary([]byte{0x01}))
	if passed != 0 {
		t.Error("repeated binary frame was not deduplicated")
	}
}

// A binary event and a JSON event must not collide just because one of the two
// content fields is empty on each.
func TestDedupMiddleware_BinaryAndJSONDoNotCollide(t *testing.T) {
	cfg := &domain.DedupConfig{Enabled: true, CacheSize: 10, TTL: "1h"}
	dedup := mustDedup(t, "test", cfg)

	var passed int
	handler := dedup(func(topic string, e domain.StreamEvent) error {
		passed++
		return nil
	})

	passed = 0
	_ = handler("topic", domain.StreamEvent{Payload: json.RawMessage(`{"id":1}`)})
	if passed != 1 {
		t.Fatal("first JSON event should pass")
	}

	passed = 0
	_ = handler("topic", domain.StreamEvent{
		ContentType: domain.ContentTypeBinary,
		Data:        []byte{0x99},
	})
	if passed != 1 {
		t.Error("binary event was dropped as a duplicate of an unrelated JSON event")
	}
}
