package pipeline

import (
	"fmt"
	"time"

	"nephtys/internal/domain"
)

// Middleware defaults. They are named here rather than inline at each use so
// validation and construction cannot drift apart about what an omitted field
// means.
const (
	defaultDedupTTL       = 1 * time.Minute
	defaultDedupCacheSize = 1000
	defaultFlushInterval  = 1 * time.Second
	defaultMaxBatchSize   = 100
)

// resolveDuration turns an optional duration field into a value. An omitted
// field takes def; a field that is present but unparseable or non-positive is
// an error.
//
// The asymmetry is deliberate: absent means "no opinion, use the default",
// while present-and-invalid means the operator stated an intent that Nephtys
// cannot honour. Substituting def there would turn a typo into a silent change
// of behavior — "5 m" flushing 300× more often than the author meant, with
// nothing in the logs. This is the only place a middleware duration is parsed,
// so no caller can reintroduce the fallback.
func resolveDuration(path, raw string, def time.Duration) (time.Duration, error) {
	if raw == "" {
		return def, nil
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a valid duration (want a unit suffix, e.g. \"500ms\", \"5s\", \"1m\")", path, raw)
	}
	if parsed <= 0 {
		return 0, fmt.Errorf("%s: must be a positive duration, got %q", path, raw)
	}
	return parsed, nil
}

// resolveCount turns an optional positive-count field into a value, applying
// def when the field is omitted (zero) and rejecting a negative value rather
// than silently replacing it with def.
func resolveCount(path string, raw, def int) (int, error) {
	if raw == 0 {
		return def, nil
	}
	if raw < 0 {
		return 0, fmt.Errorf("%s: must be a positive count, got %d", path, raw)
	}
	return raw, nil
}

// ValidateConfig reports whether a pipeline configuration is one Nephtys can
// honour exactly as written. It rejects three classes of input: malformed
// explicit values, values outside a usable range, and configuration that is
// structurally valid but cannot do anything (an enabled middleware missing the
// field it operates on, or an empty collection that builds no middleware at
// all). Errors name the offending JSON path.
//
// A nil config is a valid pass-through pipeline.
//
// Blocks are checked whether or not they carry `enabled: true`: a malformed
// duration in a disabled block is still a typo, and finding it when the config
// is written is cheaper than finding it when the block is switched on.
func ValidateConfig(cfg *domain.PipelineConfig) error {
	if cfg == nil {
		return nil
	}

	if f := cfg.Filter; f != nil {
		if len(f.MatchTypes) == 0 {
			return fmt.Errorf("pipeline.filter.match_types: must list at least one type; omit the filter block rather than leaving it empty")
		}
		for i, t := range f.MatchTypes {
			if t == "" {
				return fmt.Errorf("pipeline.filter.match_types[%d]: must not be empty", i)
			}
		}
	}

	if t := cfg.Transform; t != nil {
		if len(t.Mapping) == 0 {
			return fmt.Errorf("pipeline.transform.mapping: must contain at least one entry; omit the transform block rather than leaving it empty")
		}
		for key, path := range t.Mapping {
			if key == "" {
				return fmt.Errorf("pipeline.transform.mapping: target keys must not be empty")
			}
			if path == "" {
				return fmt.Errorf("pipeline.transform.mapping[%q]: source path must not be empty", key)
			}
		}
	}

	if e := cfg.Enrich; e != nil {
		if len(e.Tags) == 0 {
			return fmt.Errorf("pipeline.enrich.tags: must contain at least one tag; omit the enrich block rather than leaving it empty")
		}
		if _, ok := e.Tags[""]; ok {
			return fmt.Errorf("pipeline.enrich.tags: tag names must not be empty")
		}
	}

	if d := cfg.Dedup; d != nil {
		if _, err := resolveDuration("pipeline.dedup.ttl", d.TTL, defaultDedupTTL); err != nil {
			return err
		}
		if _, err := resolveCount("pipeline.dedup.cache_size", d.CacheSize, defaultDedupCacheSize); err != nil {
			return err
		}
	}

	if t := cfg.Threshold; t != nil {
		// An enabled threshold with no path builds no middleware at all, so the
		// stream reports healthy while filtering nothing the operator asked for.
		if t.Enabled && t.Path == "" {
			return fmt.Errorf("pipeline.threshold.path: required when threshold is enabled")
		}
		if t.Delta < 0 {
			return fmt.Errorf("pipeline.threshold.delta: must not be negative, got %v", t.Delta)
		}
	}

	if b := cfg.Batch; b != nil {
		if _, err := resolveDuration("pipeline.batch.flush_interval", b.FlushInterval, defaultFlushInterval); err != nil {
			return err
		}
		if _, err := resolveCount("pipeline.batch.max_batch_size", b.MaxBatchSize, defaultMaxBatchSize); err != nil {
			return err
		}
	}

	return nil
}
