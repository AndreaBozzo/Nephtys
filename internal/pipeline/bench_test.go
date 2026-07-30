package pipeline

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"nephtys/internal/domain"
)

// benchSink is a global escape hatch for the result so the compiler
// cannot dead-code-eliminate the pipeline work in benchmark loops.
var benchSink atomic.Int64

func nopSink(topic string, e domain.StreamEvent) error {
	benchSink.Add(int64(len(topic) + len(e.Payload)))
	return nil
}

// sensorPayload is a representative sensor-class JSON object that exercises
// the full enrich/transform parse-mutate-marshal path.
var sensorPayload = json.RawMessage(`{"station":"openaq-1234","pm25":17.4,` +
	`"pm10":31.2,"temp_c":22.1,"humidity":58,"wind_kmh":4.2,` +
	`"timestamp":"2026-05-09T10:15:30Z","lat":44.4949,"lon":11.3426}`)

// BenchmarkPipelineEnrich measures the cost of one Enrich pass: JSON
// unmarshal, map mutate, JSON marshal. This is the single-stage baseline.
func BenchmarkPipelineEnrich(b *testing.B) {
	pipe := mustBuild(b, context.Background(), "bench", &domain.PipelineConfig{
		Enrich: &domain.EnrichConfig{Tags: map[string]string{"env": "prod", "site": "bo1"}},
	})
	handler := pipe.Execute(nopSink)
	evt := domain.StreamEvent{Type: "sensor_reading", Payload: sensorPayload}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = handler("topic", evt)
	}
}

// BenchmarkPipelineEnrichTransform measures the cost when both Enrich and
// Transform run, since they each unmarshal+marshal independently. This is
// the worst-case JSON-cost path on the hot path today.
func BenchmarkPipelineEnrichTransform(b *testing.B) {
	pipe := mustBuild(b, context.Background(), "bench", &domain.PipelineConfig{
		Transform: &domain.TransformConfig{
			Mapping: map[string]string{"station_id": "station", "pm25_value": "pm25"},
		},
		Enrich: &domain.EnrichConfig{Tags: map[string]string{"env": "prod", "site": "bo1"}},
	})
	handler := pipe.Execute(nopSink)
	evt := domain.StreamEvent{Type: "sensor_reading", Payload: sensorPayload}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = handler("topic", evt)
	}
}

// BenchmarkPipelineDedupHit measures the dedup hot path when most events
// are duplicates (LRU mostly hit). This isolates the FNV-1a + map-lookup
// + mutex cost without conflating it with downstream stages.
func BenchmarkPipelineDedupHit(b *testing.B) {
	pipe := mustBuild(b, context.Background(), "bench", &domain.PipelineConfig{
		Dedup: &domain.DedupConfig{Enabled: true, CacheSize: 1024, TTL: "10m"},
	})
	handler := pipe.Execute(nopSink)
	evt := domain.StreamEvent{Type: "sensor_reading", Payload: sensorPayload}

	// Warm: register the payload in the cache so subsequent calls are hits.
	_ = handler("topic", evt)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = handler("topic", evt)
	}
}

// BenchmarkPipelineFullChain runs filter → transform → dedup → enrich
// end-to-end on a unique payload per iteration (forces dedup miss path).
// This is the most realistic single-event cost figure.
func BenchmarkPipelineFullChain(b *testing.B) {
	pipe := mustBuild(b, context.Background(), "bench", &domain.PipelineConfig{
		Filter:    &domain.FilterConfig{MatchTypes: []string{"sensor_reading"}},
		Transform: &domain.TransformConfig{Mapping: map[string]string{"station_id": "station"}},
		Dedup:     &domain.DedupConfig{Enabled: true, CacheSize: 100000, TTL: "10m"},
		Enrich:    &domain.EnrichConfig{Tags: map[string]string{"env": "prod"}},
	})
	handler := pipe.Execute(nopSink)

	// Pre-build unique payloads so we don't measure JSON marshal in the loop.
	payloads := make([]json.RawMessage, 1024)
	for i := range payloads {
		p, _ := json.Marshal(map[string]any{
			"station": i,
			"pm25":    17.4 + float64(i),
			"temp_c":  22.0,
		})
		payloads[i] = p
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		evt := domain.StreamEvent{Type: "sensor_reading", Payload: payloads[i%len(payloads)]}
		_ = handler("topic", evt)
	}
}
