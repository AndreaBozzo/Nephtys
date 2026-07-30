package pipeline

import (
	"strings"
	"testing"
	"time"

	"nephtys/internal/domain"
	"nephtys/internal/telemetry"
)

func TestValidateConfig_Accepts(t *testing.T) {
	tests := []struct {
		name string
		cfg  *domain.PipelineConfig
	}{
		{"nil config is a passthrough pipeline", nil},
		{"empty config", &domain.PipelineConfig{}},
		{
			"omitted durations and sizes take defaults",
			&domain.PipelineConfig{
				Dedup: &domain.DedupConfig{Enabled: true},
				Batch: &domain.BatchConfig{Enabled: true},
			},
		},
		{
			"explicit valid values",
			&domain.PipelineConfig{
				Filter:    &domain.FilterConfig{MatchTypes: []string{"trade"}},
				Transform: &domain.TransformConfig{Mapping: map[string]string{"price": "p"}},
				Enrich:    &domain.EnrichConfig{Tags: map[string]string{"env": "prod"}},
				Dedup:     &domain.DedupConfig{Enabled: true, TTL: "30s", CacheSize: 64},
				Threshold: &domain.ThresholdConfig{Enabled: true, Path: "temp", Delta: 0.5},
				Batch:     &domain.BatchConfig{Enabled: true, FlushInterval: "2s", MaxBatchSize: 50},
			},
		},
		{
			// delta 0 passes every event carrying the path. That is a thin
			// configuration but a coherent one, unlike an enabled threshold with
			// no path at all.
			"threshold with zero delta",
			&domain.PipelineConfig{Threshold: &domain.ThresholdConfig{Enabled: true, Path: "temp"}},
		},
		{
			"disabled threshold needs no path",
			&domain.PipelineConfig{Threshold: &domain.ThresholdConfig{Enabled: false}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateConfig(tt.cfg); err != nil {
				t.Errorf("ValidateConfig() = %v, want nil", err)
			}
		})
	}
}

func TestValidateConfig_Rejects(t *testing.T) {
	tests := []struct {
		name     string
		cfg      *domain.PipelineConfig
		wantPath string // the JSON path the message must name
	}{
		{
			"empty filter match_types",
			&domain.PipelineConfig{Filter: &domain.FilterConfig{MatchTypes: []string{}}},
			"pipeline.filter.match_types",
		},
		{
			"empty filter entry",
			&domain.PipelineConfig{Filter: &domain.FilterConfig{MatchTypes: []string{"trade", ""}}},
			"pipeline.filter.match_types[1]",
		},
		{
			"empty transform mapping",
			&domain.PipelineConfig{Transform: &domain.TransformConfig{Mapping: map[string]string{}}},
			"pipeline.transform.mapping",
		},
		{
			"transform mapping with empty source path",
			&domain.PipelineConfig{Transform: &domain.TransformConfig{Mapping: map[string]string{"price": ""}}},
			"pipeline.transform.mapping",
		},
		{
			"empty enrich tags",
			&domain.PipelineConfig{Enrich: &domain.EnrichConfig{Tags: map[string]string{}}},
			"pipeline.enrich.tags",
		},
		{
			"enrich tag with empty name",
			&domain.PipelineConfig{Enrich: &domain.EnrichConfig{Tags: map[string]string{"": "v"}}},
			"pipeline.enrich.tags",
		},
		{
			"unparseable dedup ttl",
			&domain.PipelineConfig{Dedup: &domain.DedupConfig{Enabled: true, TTL: "5 minutes"}},
			"pipeline.dedup.ttl",
		},
		{
			"unitless dedup ttl",
			&domain.PipelineConfig{Dedup: &domain.DedupConfig{Enabled: true, TTL: "30"}},
			"pipeline.dedup.ttl",
		},
		{
			"zero dedup ttl",
			&domain.PipelineConfig{Dedup: &domain.DedupConfig{Enabled: true, TTL: "0s"}},
			"pipeline.dedup.ttl",
		},
		{
			// A typo in a switched-off block is still a typo, and cheaper to
			// find now than the day the block is enabled.
			"malformed ttl in a disabled dedup block",
			&domain.PipelineConfig{Dedup: &domain.DedupConfig{Enabled: false, TTL: "5 minutes"}},
			"pipeline.dedup.ttl",
		},
		{
			"negative dedup cache_size",
			&domain.PipelineConfig{Dedup: &domain.DedupConfig{Enabled: true, CacheSize: -1}},
			"pipeline.dedup.cache_size",
		},
		{
			// cache_size preallocates the LRU map, so an unbounded value turns a
			// stray zero into an out-of-memory kill instead of a rejected config.
			"dedup cache_size above the ceiling",
			&domain.PipelineConfig{Dedup: &domain.DedupConfig{Enabled: true, CacheSize: maxDedupCacheSize + 1}},
			"pipeline.dedup.cache_size",
		},
		{
			"enabled threshold without a path",
			&domain.PipelineConfig{Threshold: &domain.ThresholdConfig{Enabled: true}},
			"pipeline.threshold.path",
		},
		{
			"negative threshold delta",
			&domain.PipelineConfig{Threshold: &domain.ThresholdConfig{Enabled: true, Path: "temp", Delta: -1}},
			"pipeline.threshold.delta",
		},
		{
			"unparseable batch flush_interval",
			&domain.PipelineConfig{Batch: &domain.BatchConfig{Enabled: true, FlushInterval: "1 sec"}},
			"pipeline.batch.flush_interval",
		},
		{
			"negative batch max_batch_size",
			&domain.PipelineConfig{Batch: &domain.BatchConfig{Enabled: true, MaxBatchSize: -4}},
			"pipeline.batch.max_batch_size",
		},
		{
			// max_batch_size sizes the worker's channel buffer.
			"batch max_batch_size above the ceiling",
			&domain.PipelineConfig{Batch: &domain.BatchConfig{Enabled: true, MaxBatchSize: maxBatchSize + 1}},
			"pipeline.batch.max_batch_size",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateConfig(tt.cfg)
			if err == nil {
				t.Fatalf("ValidateConfig() = nil, want an error naming %s", tt.wantPath)
			}
			if !strings.Contains(err.Error(), tt.wantPath) {
				t.Errorf("error %q does not name the offending path %q", err, tt.wantPath)
			}
		})
	}
}

// TestResolveDuration_AbsentDefaultsPresentFails pins the asymmetry the config
// contract rests on: an omitted field takes the default, a stated-but-invalid
// one is an error. Reversing the second half is the silent-fallback bug.
func TestResolveDuration_AbsentDefaultsPresentFails(t *testing.T) {
	got, err := resolveDuration("x", "", 7*time.Second)
	if err != nil || got != 7*time.Second {
		t.Errorf("omitted value = (%v, %v), want (7s, nil)", got, err)
	}

	if _, err := resolveDuration("x", "7 seconds", 7*time.Second); err == nil {
		t.Error("malformed value returned no error, so it silently took the default")
	}
}

func TestResolveCount_BoundsBothEnds(t *testing.T) {
	got, err := resolveCount("x", 0, 100, 1000)
	if err != nil || got != 100 {
		t.Errorf("omitted value = (%v, %v), want (100, nil)", got, err)
	}

	if _, err := resolveCount("x", -1, 100, 1000); err == nil {
		t.Error("negative value returned no error, so it silently took the default")
	}

	// The ceiling is what keeps the allocation sites bounded by a constant. A
	// value above it must fail rather than be handed to make().
	if _, err := resolveCount("x", 1001, 100, 1000); err == nil {
		t.Error("value above the ceiling returned no error")
	}
	if got, err := resolveCount("x", 1000, 100, 1000); err != nil || got != 1000 {
		t.Errorf("value at the ceiling = (%v, %v), want (1000, nil)", got, err)
	}
}

// TestNewDedup_RejectsMalformedTTL and its batch counterpart cover the
// constructors directly: they are the single parse site for these fields, so a
// reintroduced `if err == nil` fallback has to fail here.
func TestNewDedup_RejectsMalformedTTL(t *testing.T) {
	mw, err := NewDedup("validate-test", &domain.DedupConfig{Enabled: true, TTL: "5 minutes"})
	if err == nil {
		t.Fatal("NewDedup accepted a malformed TTL")
	}
	if mw != nil {
		t.Error("NewDedup returned a middleware alongside an error")
	}
}

func TestNewBatch_RejectsMalformedFlushInterval(t *testing.T) {
	mw, err := NewBatch(newGeneration(), &domain.BatchConfig{Enabled: true, FlushInterval: "1 sec"})
	if err == nil {
		t.Fatal("NewBatch accepted a malformed flush interval")
	}
	if mw != nil {
		t.Error("NewBatch returned a middleware alongside an error")
	}
}

// TestNewGeneration_FailedBuildLeavesNoDedupSeries covers the ordering hazard
// in the builder: dedup publishes its gauges as a side effect of being built, so
// a later stage failing would otherwise leave series behind describing a
// pipeline that never ran.
func TestNewGeneration_FailedBuildLeavesNoDedupSeries(t *testing.T) {
	streamID := "builder-test-failed-build"
	t.Cleanup(func() { telemetry.DeleteStreamSeries(streamID) })

	cfg := &domain.PipelineConfig{
		Dedup: &domain.DedupConfig{Enabled: true, CacheSize: 64},
		Batch: &domain.BatchConfig{Enabled: true, FlushInterval: "1 sec"},
	}

	gen, err := NewGeneration(streamID, cfg, nopSink)
	if err == nil {
		t.Fatal("NewGeneration accepted a malformed flush interval")
	}
	if gen != nil {
		t.Error("NewGeneration returned a generation alongside an error")
	}

	for _, name := range gatheredSeriesFor(t, streamID) {
		if strings.Contains(name, "dedup") {
			t.Errorf("dedup series %q survived a failed pipeline build", name)
		}
	}
}
