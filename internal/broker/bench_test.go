package broker

import (
	"encoding/json"
	"testing"
	"time"

	"nephtys/internal/domain"
)

// BenchmarkPublishJSON measures end-to-end JetStream publish throughput
// for a typical sensor-class JSON payload (~250 bytes).
//
// Run: go test -bench=. -benchmem ./internal/broker/...
func BenchmarkPublishJSON(b *testing.B) {
	srv := startTestBenchServer(b)

	brk, err := Connect(srv.ClientURL(), DefaultConfig())
	if err != nil {
		b.Fatalf("connect: %v", err)
	}
	defer brk.Close()

	if err := brk.EnsureStream("BENCH", []string{"bench.>"}); err != nil {
		b.Fatalf("ensure stream: %v", err)
	}

	event := domain.StreamEvent{
		Source:    "bench-source",
		Type:      "sensor_reading",
		Timestamp: time.Now().UnixMilli(),
		Payload: json.RawMessage(`{"station":"openaq-1234","pm25":17.4,` +
			`"pm10":31.2,"temp_c":22.1,"humidity":58,"wind_kmh":4.2,` +
			`"timestamp":"2026-05-09T10:15:30Z","lat":44.4949,"lon":11.3426}`),
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := brk.Publish("bench.json", event); err != nil {
			b.Fatalf("publish: %v", err)
		}
	}
}

// BenchmarkPublishBinary measures publish throughput for the binary
// (Content-Type-aware) path, which skips JSON marshalling entirely.
func BenchmarkPublishBinary(b *testing.B) {
	srv := startTestBenchServer(b)

	brk, err := Connect(srv.ClientURL(), DefaultConfig())
	if err != nil {
		b.Fatalf("connect: %v", err)
	}
	defer brk.Close()

	if err := brk.EnsureStream("BENCH", []string{"bench.>"}); err != nil {
		b.Fatalf("ensure stream: %v", err)
	}

	payload := make([]byte, 256)
	for i := range payload {
		payload[i] = byte(i)
	}
	event := domain.StreamEvent{
		Source:      "bench-source",
		Type:        "arrow-batch",
		Timestamp:   time.Now().UnixMilli(),
		Seq:         1,
		ContentType: domain.ContentTypeArrowStream,
		Data:        payload,
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := brk.Publish("bench.bin", event); err != nil {
			b.Fatalf("publish: %v", err)
		}
	}
}
