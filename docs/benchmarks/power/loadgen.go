// loadgen — exact-rate load generator for Nephtys power/throughput benchmarks.
//
// Runs on the sampling host, NOT on the device under test: the edge node must do
// only the connector's work, while event generation stays external. This keeps the
// measured wall power attributable to Nephtys rather than the generator.
//
// Endpoints:
//
//	/sse?rate=N   SSE stream at N events/second (exact, declarable rate)
//	/data         single JSON payload (for rest_poller sources)
//	/stats        counters: events sent, connections, effective rate
//	/reset        zero the counters (call at the start of a measured window)
//
// Usage: go run loadgen.go [-addr :8099]
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"
)

var (
	eventsSent  atomic.Uint64
	connections atomic.Int64
	startTime   = time.Now()
)

// payload is a deliberately generic sensor reading, consistent with Nephtys'
// generic-first rule (no source-specific shape).
type payload struct {
	SensorID    string  `json:"sensor_id"`
	Seq         uint64  `json:"seq"`
	Timestamp   int64   `json:"timestamp"`
	Temperature float64 `json:"temperature_c"`
	Humidity    float64 `json:"humidity_pct"`
	Pressure    float64 `json:"pressure_hpa"`
	Status      string  `json:"status"`
}

func newPayload(seq uint64) payload {
	return payload{
		SensorID:    "loadgen-01",
		Seq:         seq,
		Timestamp:   time.Now().UnixMilli(),
		Temperature: 20.0 + float64(seq%100)/10.0,
		Humidity:    45.0 + float64(seq%50)/10.0,
		Pressure:    1013.0 + float64(seq%20)/10.0,
		Status:      "ok",
	}
}

// handleSSE pushes events at a constant rate. To stay stable on coarse OS timer
// granularity it does not use one tick per event: it ticks every 10 ms and emits
// rate/100 events per tick, distributing the remainder to hit the exact mean rate.
func handleSSE(w http.ResponseWriter, r *http.Request) {
	rate := 10
	if v := r.URL.Query().Get("rate"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			rate = n
		}
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	connections.Add(1)
	defer connections.Add(-1)
	log.Printf("SSE connection opened from %s, rate=%d ev/s", r.RemoteAddr, rate)

	const tickHz = 100 // tick every 10 ms
	perTick := rate / tickHz
	remainder := rate % tickHz

	ticker := time.NewTicker(time.Second / tickHz)
	defer ticker.Stop()

	var seq uint64
	var acc int

	for {
		select {
		case <-r.Context().Done():
			log.Printf("SSE connection closed from %s after %d events", r.RemoteAddr, seq)
			return
		case <-ticker.C:
			n := perTick
			acc += remainder
			if acc >= tickHz {
				n++
				acc -= tickHz
			}
			for i := 0; i < n; i++ {
				seq++
				b, _ := json.Marshal(newPayload(seq))
				if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
					return
				}
				eventsSent.Add(1)
			}
			if n > 0 {
				flusher.Flush()
			}
		}
	}
}

func handleData(w http.ResponseWriter, _ *http.Request) {
	seq := eventsSent.Add(1)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(newPayload(seq))
}

func handleStats(w http.ResponseWriter, _ *http.Request) {
	sent := eventsSent.Load()
	elapsed := time.Since(startTime).Seconds()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"events_sent":       sent,
		"connections":       connections.Load(),
		"uptime_s":          elapsed,
		"avg_rate_events_s": float64(sent) / elapsed,
	})
}

func handleReset(w http.ResponseWriter, _ *http.Request) {
	eventsSent.Store(0)
	startTime = time.Now()
	_, _ = w.Write([]byte(`{"reset":true}`))
}

func main() {
	addr := flag.String("addr", ":8099", "listen address")
	flag.Parse()

	http.HandleFunc("/sse", handleSSE)
	http.HandleFunc("/data", handleData)
	http.HandleFunc("/stats", handleStats)
	http.HandleFunc("/reset", handleReset)

	log.Printf("loadgen listening on %s", *addr)
	log.Printf("  SSE   : http://<ip>%s/sse?rate=N", *addr)
	log.Printf("  stats : http://<ip>%s/stats", *addr)
	if err := http.ListenAndServe(*addr, nil); err != nil {
		log.Fatal(err)
	}
}
