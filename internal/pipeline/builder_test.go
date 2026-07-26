package pipeline

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"nephtys/internal/domain"
	"nephtys/internal/telemetry"
)

// gatheredSeriesFor returns the names of gathered metric families carrying the
// given stream_id. It reads the registry rather than calling WithLabelValues,
// which would create the very child we are asserting is absent.
func gatheredSeriesFor(t *testing.T, streamID string) []string {
	t.Helper()

	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	var found []string
	for _, f := range families {
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "stream_id" && l.GetValue() == streamID {
					found = append(found, f.GetName())
				}
			}
		}
	}
	sort.Strings(found)
	return found
}

// TestBuildFromConfigClearsDedupSeriesWhenDedupDropped covers a pipeline
// hot-swap that removes dedup from a running stream. The cache gauges are set
// when the middleware is built and nothing else ever updates them, so without
// an explicit clear they would keep reporting the occupancy and capacity of a
// middleware that is no longer in the chain.
func TestBuildFromConfigClearsDedupSeriesWhenDedupDropped(t *testing.T) {
	streamID := "builder-test-dedup-swap"
	t.Cleanup(func() { telemetry.DeleteStreamSeries(streamID) })

	withDedup := &domain.PipelineConfig{
		Dedup: &domain.DedupConfig{Enabled: true, CacheSize: 64},
	}
	BuildFromConfig(context.Background(), streamID, withDedup)

	before := gatheredSeriesFor(t, streamID)
	if !contains(before, "nephtys_dedup_cache_capacity") {
		t.Fatalf("capacity gauge missing after building with dedup, got %v", before)
	}

	// Hot-swap to a pipeline without dedup.
	withoutDedup := &domain.PipelineConfig{
		Enrich: &domain.EnrichConfig{Tags: map[string]string{"env": "test"}},
	}
	BuildFromConfig(context.Background(), streamID, withoutDedup)

	for _, name := range gatheredSeriesFor(t, streamID) {
		if strings.Contains(name, "dedup") {
			t.Errorf("dedup series %q survived a pipeline swap that dropped dedup", name)
		}
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
